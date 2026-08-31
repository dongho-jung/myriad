package myriad

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func launchForTask(store *Store, task Record, command []string, integrate bool, taskLocked bool) (int, error) {
	var taskLock *fileLock
	var err error
	if !taskLocked {
		taskLock, err = store.Lock("task:"+stringValue(task, "task_id"), true)
		if err != nil {
			return 2, err
		}
		defer taskLock.Unlock()
	}
	current, err := store.Load(stringValue(task, "task_id"))
	if err != nil {
		return 2, err
	}
	replaceRecord(task, current)
	worktree, err := managedWorktreePath(store, task)
	if err != nil {
		return 2, err
	}
	workingDirectory, err := managedAgentWorkingDirectory(task, command)
	if err != nil {
		return 2, err
	}
	identity, _ := taskCheckoutIdentity(task)
	reservation, err := acquireCheckoutSession(store, worktree, true, sessionOptions{
		Agent: stringValue(task, "agent"), TaskID: stringValue(task, "task_id"),
		WorkingDirectory: workingDirectory, Repository: stringValue(task, "repository"),
		Identity: identity, GitCommonDir: stringValue(task, "git_common_dir"),
		BaseSHA:      firstNonempty(currentHead(task), stringValue(task, "base_sha")),
		SourceBranch: firstNonempty(stringValue(task, "branch"), stringValue(task, "source_branch")),
	})
	if err != nil {
		return 2, err
	}
	if reservation == nil {
		return 2, fail("worktree already has an active agent: %s", worktree)
	}
	sessionID := reservation.SessionID
	exitCode, launchErr := taskLaunch(store, task, command, reservation)
	handoffTaskIDs := reservation.CaptureHandoffTasks()
	reservation.Release(store, worktree, identity)
	if launchErr != nil {
		task["launcher_exception"] = Record{"at": now(), "message": launchErr.Error(), "type": fmt.Sprintf("%T", launchErr)}
		if processAlive(task["process"]) {
			_ = setStatus(store, task, StatusRunning, "launcher ended while the coding agent was still active")
		} else {
			delete(task, "process")
			if interruptedTaskHasNoRepositoryWork(task) {
				_ = setStatus(store, task, StatusFailed, "agent launch failed: "+launchErr.Error())
				_, _ = cleanupTask(store, task, false, false)
			} else {
				preserveInterruptedTask(store, task, "launcher ended unexpectedly: "+launchErr.Error()+"; resume required")
			}
		}
		return exitCode, launchErr
	}
	if exitCode == handoffExitCode && len(handoffTaskIDs) > 0 {
		recordAgentExit(task, 0, false)
		task["handoff_completed_at"] = now()
		_ = store.Save(task)
		exitCode = 0
	}
	if err := finalizeTask(store, task, integrate, false); err != nil {
		return exitCode, err
	}
	attachmentResults := finalizeSessionAttachments(store, sessionID, taskExitCode(task), boolValue(task, "agent_exit_graceful", false))
	if len(attachmentResults) > 0 {
		ids := []any{}
		failures := []any{}
		for _, result := range attachmentResults {
			ids = append(ids, result["task_id"])
			status := stringValue(result, "status")
			if status != StatusIntegrated && status != StatusCompleted && !(status == StatusReady && !boolValue(result, "auto_integrate", true)) {
				failures = append(failures, result)
			}
		}
		task["attachments"] = ids
		if len(failures) > 0 {
			task["attachment_failures"] = failures
		} else {
			delete(task, "attachment_failures")
		}
		_ = store.Save(task)
	}
	if len(handoffTaskIDs) > 0 {
		retryHandoffIntegrations(store, handoffTaskIDs)
	}
	retryReadyIntegrations(store, stringValue(task, "repository"), []string{stringValue(task, "task_id")})
	if boolValue(task, "agent_exit_graceful", false) {
		return 0, nil
	}
	return exitCode, nil
}

func preserveInterruptedTask(store *Store, task Record, reason string) {
	delete(task, "process")
	head := currentHead(task)
	if head != "" && head != stringValue(task, "base_sha") && isAncestor(stringValue(task, "repository"), stringValue(task, "base_sha"), head) {
		task["result_commit"] = head
	}
	task["interrupted_at"] = now()
	_ = setStatus(store, task, StatusRecovery, reason)
}

func interruptedTaskHasNoRepositoryWork(task Record) bool {
	path := stringValue(task, "worktree_path")
	if path == "" {
		return false
	}
	if !taskWorktreeReady(task) {
		if entries, err := os.ReadDir(path); err == nil && len(entries) > 0 {
			return false
		}
		head := currentHead(task)
		return head == "" || head == stringValue(task, "base_sha")
	}
	changes, err := worktreeChanges(path)
	return err == nil && len(changes.Normal) == 0 && len(changes.Ignored) == 0 && currentHead(task) == stringValue(task, "base_sha")
}

func completeEmptyInterruptedTask(store *Store, task Record) bool {
	delete(task, "process")
	delete(task, "agent_exit_code")
	delete(task, "agent_exit_graceful")
	delete(task, "interrupted_at")
	if inspectResult(store, task, false) != nil || stringValue(task, "status") != StatusCompleted {
		return false
	}
	task["empty_interruption_resolved_at"] = now()
	_ = setStatus(store, task, StatusCompleted, "interrupted session had no repository changes; cleaned automatically")
	return true
}

func taskBelongsToRepository(task Record, common string) bool {
	recorded, _ := canonical(stringValue(task, "git_common_dir"))
	common, _ = canonical(common)
	return recorded != "" && recorded == common
}

func refreshInterruptedTasks(store *Store, repository string) []Record {
	pruneDeadSessionMetadata(store)
	common, _ := gitCommonDir(repository)
	for _, snapshot := range store.All(true) {
		if !taskBelongsToRepository(snapshot, common) {
			continue
		}
		status := stringValue(snapshot, "status")
		interruptedRecovery := status == StatusRecovery && (stringValue(snapshot, "interrupted_at") != "" || !taskWorktreeReady(snapshot))
		if status != StatusCreated && status != StatusRunning && !interruptedRecovery {
			continue
		}
		if processAlive(snapshot["process"]) {
			continue
		}
		lock, err := store.Lock("task:"+stringValue(snapshot, "task_id"), false)
		if err != nil {
			continue
		}
		current, loadErr := store.Load(stringValue(snapshot, "task_id"))
		if loadErr == nil && !processAlive(current["process"]) {
			if interruptedTaskHasNoRepositoryWork(current) {
				completeEmptyInterruptedTask(store, current)
			} else if stringValue(current, "status") == StatusCreated || stringValue(current, "status") == StatusRunning {
				preserveInterruptedTask(store, current, "agent process ended before lifecycle completion; resume required")
			}
		}
		_ = lock.Unlock()
	}
	result := []Record{}
	for _, task := range store.All(true) {
		if taskBelongsToRepository(task, common) {
			result = append(result, task)
		}
	}
	return result
}

func recoveryChangedPaths(task Record) []string {
	changes, err := worktreeChanges(stringValue(task, "worktree_path"))
	if err != nil {
		return nil
	}
	paths := []string{}
	for _, line := range changes.Normal {
		path := line
		if len(path) > 3 {
			path = path[3:]
		}
		if _, after, found := strings.Cut(path, " -> "); found {
			path = after
		}
		paths = append(paths, path)
	}
	return paths
}

func recoveryTaskTitle(task Record) string {
	if stored := storedTaskTitle(task); stored != "" {
		return stored
	}
	head := currentHead(task)
	if head != "" && head != stringValue(task, "base_sha") {
		result, _ := gitCommand(stringValue(task, "repository"), false, "log", "-1", "--format=%s", head)
		if subject := strings.TrimSpace(result.Stdout); subject != "" {
			return subject
		}
	}
	paths := recoveryChangedPaths(task)
	if len(paths) > 0 {
		visible := paths[:min(3, len(paths))]
		suffix := ""
		if len(paths) > 3 {
			suffix = fmt.Sprintf(" +%d", len(paths)-3)
		}
		return "changes in " + strings.Join(visible, ", ") + suffix
	}
	return "untitled recovery"
}

func prepareRecovery(task Record) string {
	notes := []string{}
	quarantines := recordSlice(task, "worktree_quarantines")
	if len(quarantines) > 0 {
		latest := anyRecord(quarantines[len(quarantines)-1])
		if path := stringValue(latest, "path"); path != "" {
			notes = append(notes, "The previous managed path was preserved at "+path+"; inspect it for files that must be recovered.")
		}
	}
	if warning := stringValue(task, "memory_warning"); warning != "" {
		notes = append(notes, "A non-blocking "+MemoryName+" proposal is recorded: "+warning+".")
	}
	if !taskWorktreeReady(task) {
		notes = append(notes, "The managed worktree was not created before the previous session ended. Send the next prompt to provision it from the recorded base.")
		return strings.Join(notes, " ")
	}
	path := stringValue(task, "worktree_path")
	repository := stringValue(task, "repository")
	head, _ := gitRef(path, "HEAD")
	target := stringValue(task, "target_branch")
	excluded := stringValue(task, "base_sha")
	if target != "" && branchExists(repository, target) {
		excluded, _ = gitRef(repository, "refs/heads/"+target)
	}
	if findings, _ := forbiddenHistory(repository, head, excluded); len(findings) > 0 {
		notes = append(notes, "Rewrite unpublished commits so these machine-local paths never appear in history: "+strings.Join(findingPaths(findings), ", ")+".")
	}
	if stringValue(task, "interrupted_at") != "" {
		notes = append(notes, "Resume the interrupted session and continue from its preserved files and commits.")
		return strings.Join(notes, " ")
	}
	changes, _ := worktreeChanges(path)
	if len(changes.Normal) > 0 || target == "" {
		notes = append(notes, "Resume the preserved files and commit the completed result.")
		return strings.Join(notes, " ")
	}
	if !branchExists(repository, target) {
		notes = append(notes, "The target branch "+target+" no longer exists; repair the task metadata first.")
		return strings.Join(notes, " ")
	}
	mergeHead, _ := gitCommand(path, false, "rev-parse", "--verify", "MERGE_HEAD")
	if mergeHead.ExitCode == 0 {
		notes = append(notes, "A target merge is already in progress. Resolve only those conflicts and commit.")
		return strings.Join(notes, " ")
	}
	targetSHA, _ := gitRef(repository, "refs/heads/"+target)
	if isAncestor(repository, targetSHA, head) {
		notes = append(notes, "The task already contains the current target; finish and commit the result.")
		return strings.Join(notes, " ")
	}
	merged, _ := gitCommand(path, false, "-c", "core.hooksPath=/dev/null", "merge", "--no-ff", "--no-commit", targetSHA)
	if merged.ExitCode != 0 {
		notes = append(notes, "Myriad prepared merge conflicts with the current target. Resolve only those conflicts and commit.")
	} else {
		notes = append(notes, "Myriad staged the current target merge. Validate, make any needed fix, and commit it.")
	}
	return strings.Join(notes, " ")
}

func defaultRecoveryCommand(agent, prompt string) ([]string, error) {
	if agent == "codex" {
		return []string{"codex", "resume", "--last", "--dangerously-bypass-approvals-and-sandbox", prompt}, nil
	}
	if agent == "claude" {
		return []string{"env", "IS_DEMO=1", "claude", "--continue", "--ide", "--chrome", "--allow-dangerously-skip-permissions", "--effort", "max", "--permission-mode", "bypassPermissions", prompt}, nil
	}
	return nil, fail("custom agents require a recovery command after --")
}

func recoverTask(store *Store, taskID, agent string, integrationPolicy *bool, newSession bool, prompt string, command []string, quiet bool) (int, error) {
	lock, err := store.Lock("task:"+taskID, false)
	if err != nil {
		return 2, err
	}
	defer lock.Unlock()
	task, err := store.Load(taskID)
	if err != nil {
		return 2, err
	}
	if processAlive(task["process"]) {
		return 2, fail("coding agent is still running")
	}
	if stringValue(task, "integrated_commit") != "" {
		return 2, fail("result is already integrated; use cleanup for retained artifacts")
	}
	if err := recreateWorktree(store, task); err != nil {
		return 2, err
	}
	context := prepareRecovery(task)
	if agent == "" {
		agent = firstNonempty(stringValue(task, "agent"), "codex")
	}
	if agent == "custom" && len(command) == 0 {
		return 2, fail("custom agents require a command after --")
	}
	task["agent"] = agent
	if integrationPolicy != nil {
		task["auto_integrate"] = *integrationPolicy
	} else if _, exists := task["auto_integrate"]; !exists {
		task["auto_integrate"] = true
	}
	task["resume_hint"] = resumeHint(agent)
	attempts, _ := intValue(task["recovery_attempts"])
	task["recovery_attempts"] = attempts + 1
	fullPrompt := fmt.Sprintf("Recover preserved task %q.\n\n%s", recoveryTaskTitle(task), context)
	if prompt != "" {
		fullPrompt += "\n\nAdditional instruction: " + prompt
	}
	if len(command) == 0 {
		if newSession {
			command, err = defaultAgentCommand(agent, fullPrompt)
		} else {
			command, err = defaultRecoveryCommand(agent, fullPrompt)
			if err == nil && agent == "codex" {
				recoveryCWD := taskWorkingDirectory(task)
				if !taskWorktreeReady(task) {
					recoveryCWD, _ = taskOriginWorkingDirectory(task)
				}
				command, err = markCodexRecoveryCommand(command, recoveryCWD)
			}
		}
		if err != nil {
			return 2, err
		}
	}
	delete(task, "interrupted_at")
	if err := store.Save(task); err != nil {
		return 2, err
	}
	exitCode, err := launchForTask(store, task, command, boolValue(task, "auto_integrate", true), true)
	if !quiet {
		fmt.Printf("%s: %s\n", taskID, stringValue(task, "status"))
	}
	if err != nil {
		return exitCode, err
	}
	if stringValue(task, "integrated_commit") != "" || stringValue(task, "status") == StatusCompleted || (!boolValue(task, "auto_integrate", true) && stringValue(task, "status") == StatusReady) {
		return 0, nil
	}
	if exitCode == 0 {
		exitCode = 2
	}
	return exitCode, nil
}

func clearInterruptedIntegration(store *Store, task Record) {
	terminateOwnedProcess(task["validation_process"])
	candidate := stringValue(task, "integration_candidate")
	if candidate != "" {
		candidate, _ = canonical(candidate)
		root, _ := canonical(store.Integrations)
		if isWithin(candidate, root) {
			if removeIntegrationWorktree(stringValue(task, "repository"), candidate) {
				delete(task, "integration_cleanup_warning")
			} else {
				task["integration_cleanup_warning"] = "integration worktree cleanup failed: " + candidate
			}
		} else {
			task["integration_cleanup_warning"] = "refused unexpected integration path: " + candidate
		}
	}
	delete(task, "validation_process")
	delete(task, "integration_process")
	delete(task, "integration_candidate")
	_ = store.Save(task)
}

func recognizeResultOnTarget(store *Store, task Record) bool {
	repository, target, result := stringValue(task, "repository"), stringValue(task, "target_branch"), stringValue(task, "result_commit")
	if repository == "" || target == "" || result == "" || !branchExists(repository, target) {
		return false
	}
	targetSHA, _ := gitRef(repository, "refs/heads/"+target)
	if !isAncestor(repository, result, targetSHA) {
		return false
	}
	task["integrated_commit"] = targetSHA
	delete(task, "unowned_integration_interrupted")
	_ = setStatus(store, task, StatusIntegrated, "recorded result is already present on target")
	resolveTaskNotices(store, stringValue(task, "task_id"))
	_ = applyMemoryUpdate(store, task)
	_, _ = cleanupTask(store, task, false, false)
	return true
}

func integrateTaskCommand(store *Store, taskID string, quiet bool) (int, error) {
	lock, err := store.Lock("task:"+taskID, false)
	if err != nil {
		return 2, err
	}
	defer lock.Unlock()
	task, err := store.Load(taskID)
	if err != nil {
		return 2, err
	}
	task["auto_integrate"] = true
	_ = store.Save(task)
	if processAlive(task["process"]) {
		return 2, fail("coding agent is still running")
	}
	status := stringValue(task, "status")
	if status == StatusIntegrating || status == StatusValidating {
		if processAlive(task["integration_process"]) {
			return 2, fail("integration is still running")
		}
		clearInterruptedIntegration(store, task)
		_ = setStatus(store, task, StatusReady, "operator approved interrupted integration retry")
	}
	if stringValue(task, "result_commit") == "" {
		_ = inspectResult(store, task, true)
	}
	success := false
	if stringValue(task, "status") == StatusIntegrated {
		resolveTaskNotices(store, taskID)
		success = true
	} else if stringValue(task, "status") == StatusReady {
		success = integrateTask(store, task)
	}
	if !quiet {
		fmt.Printf("%s: %s\n", taskID, stringValue(task, "status"))
	}
	if success {
		return 0, nil
	}
	return 2, nil
}

func retryHandoffIntegrations(store *Store, taskIDs []string) bool {
	seen := map[string]struct{}{}
	success := true
	for _, taskID := range taskIDs {
		if _, exists := seen[taskID]; exists {
			continue
		}
		seen[taskID] = struct{}{}
		result, err := integrateTaskCommand(store, taskID, true)
		if err != nil || result != 0 {
			fmt.Fprintf(os.Stderr, "myriad: handoff integration for %s remains queued: %v\n", taskID, err)
			success = false
		}
	}
	return success
}

func retryReadyIntegrations(store *Store, repository string, excluded []string) bool {
	blocked := map[string]struct{}{}
	for _, taskID := range excluded {
		blocked[taskID] = struct{}{}
	}
	common, _ := gitCommonDir(repository)
	tasks := store.All(false)
	sort.SliceStable(tasks, func(i, j int) bool { return stringValue(tasks[i], "created_at") < stringValue(tasks[j], "created_at") })
	ids := []string{}
	for _, task := range tasks {
		if _, skip := blocked[stringValue(task, "task_id")]; !skip && taskBelongsToRepository(task, common) && stringValue(task, "status") == StatusReady && boolValue(task, "auto_integrate", true) && !processAlive(task["process"]) {
			ids = append(ids, stringValue(task, "task_id"))
		}
	}
	return retryHandoffIntegrations(store, ids)
}

func attachmentTasks(store *Store, sessionID string) []Record {
	result := []Record{}
	for _, task := range store.All(false) {
		if stringValue(task, "attachment_session_id") == sessionID {
			result = append(result, task)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		return stringValue(result[i], "created_at") < stringValue(result[j], "created_at")
	})
	return result
}

func finalizeSessionAttachments(store *Store, sessionID string, agentExitCode int, graceful bool) []Record {
	results := []Record{}
	for _, snapshot := range attachmentTasks(store, sessionID) {
		lock, err := store.Lock("task:"+stringValue(snapshot, "task_id"), false)
		if err != nil {
			results = append(results, Record{"task_id": snapshot["task_id"], "status": snapshot["status"], "auto_integrate": boolValue(snapshot, "auto_integrate", true), "reason": "attachment lifecycle is busy"})
			continue
		}
		task, loadErr := store.Load(stringValue(snapshot, "task_id"))
		if loadErr == nil && (stringValue(task, "status") == StatusCreated || stringValue(task, "status") == StatusRunning) {
			delete(task, "process")
			recordAgentExit(task, agentExitCode, graceful)
			task["attachment_finished_at"] = now()
			_ = store.Save(task)
			_ = finalizeTask(store, task, boolValue(task, "auto_integrate", true), false)
		}
		if loadErr == nil {
			results = append(results, Record{"task_id": task["task_id"], "status": task["status"], "auto_integrate": boolValue(task, "auto_integrate", true)})
			fmt.Printf("attachment %s: %s\n", stringValue(task, "task_id"), stringValue(task, "status"))
		}
		_ = lock.Unlock()
	}
	return results
}

func startAttachmentLease(store *Store, task Record, owner Record) error {
	worktree := stringValue(task, "worktree_path")
	identity, _ := taskCheckoutIdentity(task)
	lock, err := store.CheckoutLock(worktree, identity, false)
	if err != nil {
		return err
	}
	executable, err := executablePath()
	if err != nil {
		lock.Unlock()
		return err
	}
	ownerPID, _ := intValue(owner["pid"])
	cmd := exec.Command(executable, internalLease)
	cmd.ExtraFiles = []*os.File{lock.File}
	cmd.Env = overlayEnvironment(os.Environ(), map[string]string{
		envInheritedLockFDs:        "3",
		"MYRIAD_LEASE_OWNER_PID":   strconv.Itoa(ownerPID),
		"MYRIAD_LEASE_OWNER_START": stringValue(owner, "start"),
	})
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		lock.Unlock()
		return err
	}
	_ = lock.CloseWithoutUnlock()
	_ = cmd.Process.Release()
	return nil
}

func attachmentLease() (int, error) {
	descriptors, err := inheritedDescriptors()
	if err != nil || len(descriptors) != 1 {
		return 2, fail("attachment lease received an invalid checkout lock")
	}
	defer closeDescriptors(descriptors)
	ownerPID, err := strconv.Atoi(os.Getenv("MYRIAD_LEASE_OWNER_PID"))
	if err != nil || ownerPID <= 1 {
		return 2, fail("attachment lease has no valid owner")
	}
	ownerStart := os.Getenv("MYRIAD_LEASE_OWNER_START")
	for processStart(ownerPID) == ownerStart && ownerStart != "" {
		time.Sleep(100 * time.Millisecond)
	}
	return 0, nil
}

func attachRepository(store *Store, requested string) error {
	sessionID, _, session, err := currentAgentSession(store, "")
	if err != nil {
		return err
	}
	parentID := stringValue(session, "task_id")
	if parentID == "" {
		return fail("secondary repositories can only attach to a managed task")
	}
	parent, err := store.Load(parentID)
	if err != nil {
		return err
	}
	owner, parentOwner := recordMap(session, "process"), recordMap(parent, "process")
	if owner == nil || stringValue(owner, "role") != "lock-supervisor" || !processAlive(owner) || !processIdentityEqual(owner, parentOwner) {
		return fail("managed task supervisor is no longer active")
	}
	requested, _ = canonical(requested)
	if info, err := os.Stat(requested); err != nil || !info.IsDir() {
		return fail("secondary repository path does not exist: %s", requested)
	}
	checkout, err := repoRoot(requested)
	if err != nil {
		return err
	}
	repository, err := primaryWorktree(checkout)
	if err != nil {
		return err
	}
	common, _ := gitCommonDir(repository)
	if common == stringValue(parent, "git_common_dir") {
		return fail("the requested path belongs to the task's existing repository")
	}
	lock, err := store.Lock("attachment:"+sessionID+":"+common, true)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	for _, existing := range attachmentTasks(store, sessionID) {
		if stringValue(existing, "git_common_dir") == common && (stringValue(existing, "status") == StatusCreated || stringValue(existing, "status") == StatusRunning) {
			if info, err := os.Stat(stringValue(existing, "worktree_path")); err == nil && info.IsDir() {
				fmt.Printf("task: %s\nworktree: %s\nbranch: %s\n", stringValue(existing, "task_id"), stringValue(existing, "worktree_path"), stringValue(existing, "branch"))
				return nil
			}
		}
	}
	activity, err := store.RepositoryActivityLock(repository, false, true)
	if err != nil {
		return err
	}
	task, createErr := createTask(store, createTaskOptions{
		LaunchCWD: requested, Agent: firstNonempty(stringValue(parent, "agent"), stringValue(session, "agent"), "codex"),
		NoIntegrate: !boolValue(parent, "auto_integrate", true),
		Description: "secondary repository attached to " + parentID,
		TaskSlug:    stringValue(parent, "provisioning_slug"),
	})
	if createErr != nil {
		activity.Unlock()
		return createErr
	}
	task["attachment_session_id"] = sessionID
	task["attachment_parent_task_id"] = parentID
	task["attachment_source_path"] = requested
	task["process"] = cloneRecord(owner)
	_ = setStatus(store, task, StatusRunning, "")
	if err := startAttachmentLease(store, task, owner); err != nil {
		activity.Unlock()
		_ = setStatus(store, task, StatusRecovery, "attachment checkout lease failed: "+err.Error())
		return err
	}
	_ = activity.Unlock()
	fmt.Printf("task: %s\nworktree: %s\nbranch: %s\n", stringValue(task, "task_id"), stringValue(task, "worktree_path"), stringValue(task, "branch"))
	return nil
}

func recordOrphans(store *Store) {
	known := map[string]struct{}{}
	for _, task := range store.All(false) {
		if path := stringValue(task, "worktree_path"); path != "" {
			resolved, _ := canonical(path)
			known[resolved] = struct{}{}
		}
	}
	repositories, _ := os.ReadDir(store.Worktrees)
	for _, repositoryEntry := range repositories {
		if !repositoryEntry.IsDir() {
			continue
		}
		paths, _ := os.ReadDir(filepath.Join(store.Worktrees, repositoryEntry.Name()))
		for _, entry := range paths {
			path := filepath.Join(store.Worktrees, repositoryEntry.Name(), entry.Name())
			resolved, _ := canonical(path)
			if !entry.IsDir() {
				continue
			}
			if _, exists := known[resolved]; exists {
				continue
			}
			if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
				continue
			}
			common, commonErr := gitCommonDir(path)
			repository, rootErr := primaryWorktree(path)
			head, headErr := gitRef(path, "HEAD")
			branch, _ := gitCommand(path, false, "branch", "--show-current")
			if commonErr != nil || rootErr != nil || headErr != nil {
				continue
			}
			taskID := "orphan-" + sha256Hex([]byte(path))[:12]
			task := Record{
				"schema_version": TaskRecordSchema, "task_id": taskID,
				"repository": repository, "git_common_dir": common,
				"base_sha": head, "branch": strings.TrimSpace(branch.Stdout),
				"target_branch": nil, "worktree_path": path, "workdir_relative": ".",
				"worktree_state": worktreeReady, "agent": "unknown",
				"description": "unregistered managed worktree", "result_commit": head,
				"status": StatusRecovery, "status_reason": "orphan discovered; preserved for operator inspection",
				"created_at": now(),
			}
			_ = store.Save(task)
		}
	}
}

func reconcileOne(store *Store, task Record, integrate bool) {
	if processAlive(task["process"]) || processAlive(task["integration_process"]) {
		return
	}
	status := stringValue(task, "status")
	if stringValue(task, "integration_candidate") != "" && status != StatusIntegrating && status != StatusValidating {
		clearInterruptedIntegration(store, task)
	}
	switch status {
	case StatusRunning:
		if stringValue(task, "attachment_session_id") != "" {
			delete(task, "process")
			task["attachment_reconciled_at"] = now()
			recordAgentExit(task, 0, false)
			_ = store.Save(task)
			_ = finalizeTask(store, task, integrate && boolValue(task, "auto_integrate", true), true)
		} else {
			preserveInterruptedTask(store, task, "agent process ended before lifecycle completion; resume required")
		}
	case StatusCreated:
		preserveInterruptedTask(store, task, "agent process ended before lifecycle completion; resume required")
	case StatusIntegrating, StatusValidating:
		clearInterruptedIntegration(store, task)
		if recognizeResultOnTarget(store, task) {
			return
		}
		_ = setStatus(store, task, StatusReady, "interrupted integration reset and queued")
		if integrate && boolValue(task, "auto_integrate", true) {
			integrateTask(store, task)
		}
	case StatusReady:
		if integrate && boolValue(task, "auto_integrate", true) {
			integrateTask(store, task)
		}
	case StatusRecovery:
		if stringValue(task, "result_commit") != "" {
			recognizeResultOnTarget(store, task)
		}
	case StatusIntegrated, StatusCompleted, StatusFailed:
		_, _ = cleanupTask(store, task, false, false)
	}
}

func reconcile(store *Store, integrate, quiet bool) int {
	lock, err := store.Lock("reconcile", false)
	if err != nil {
		if isLockBusy(err) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "myriad:", err)
		return 2
	}
	defer lock.Unlock()
	pruneDeadSessionMetadata(store)
	recordOrphans(store)
	failed := false
	repositories := map[string]struct{}{}
	for _, snapshot := range store.All(true) {
		taskLock, err := store.Lock("task:"+stringValue(snapshot, "task_id"), false)
		if err != nil {
			continue
		}
		task, loadErr := store.Load(stringValue(snapshot, "task_id"))
		if loadErr == nil {
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						failed = true
						task["reconcile_error"] = fmt.Sprint(recovered)
						_ = store.Save(task)
					}
				}()
				reconcileOne(store, task, integrate)
			}()
			repositories[stringValue(task, "repository")] = struct{}{}
		}
		_ = taskLock.Unlock()
	}
	for repository := range repositories {
		if info, err := os.Stat(repository); err == nil && info.IsDir() {
			_, _ = gitCommand(repository, false, "worktree", "prune")
		}
	}
	recovery := 0
	for _, task := range store.All(false) {
		if stringValue(task, "status") == StatusRecovery {
			recovery++
		}
	}
	if recovery > 0 {
		failed = true
		fmt.Fprintf(os.Stderr, "myriad: %d task(s) require recovery\n", recovery)
	}
	if !quiet {
		printTaskList(store)
	}
	if failed {
		return 2
	}
	return 0
}

func readNativeMemory(store *Store, repository, checkout string) (Record, error) {
	current, _ := gitCommand(checkout, true, "branch", "--show-current")
	canonicalPath, canonicalMemory, err := ensureMemory(store, repository, strings.TrimSpace(current.Stdout))
	if err != nil {
		return nil, err
	}
	localPath := filepath.Join(checkout, MemoryName)
	secondary := filepath.Dir(canonicalPath) != checkout
	if secondary {
		value := canonicalMemory
		if _, err := os.Stat(localPath); err == nil {
			local, err := readMemory(localPath)
			if err != nil {
				return nil, err
			}
			value = overlayJSON(canonicalMemory, local).(Record)
			recordMap(value, "settings")["integration_target"] = recordMap(canonicalMemory, "settings")["integration_target"]
		}
		if err := writeMemory(localPath, value); err != nil {
			return nil, err
		}
	}
	return Record{"base": canonicalMemory, "canonical_path": canonicalPath, "local_path": localPath, "secondary": secondary}, nil
}

func overlayJSON(current, local any) any {
	currentMap, currentOK := toAnyMap(current)
	localMap, localOK := toAnyMap(local)
	if currentOK && localOK {
		result := cloneRecord(currentMap)
		for key, value := range localMap {
			if existing, exists := result[key]; exists {
				result[key] = overlayJSON(existing, value)
			} else {
				result[key] = value
			}
		}
		return result
	}
	return local
}

func finalizeNativeMemory(store *Store, session Record) {
	if !boolValue(session, "secondary", false) {
		if _, err := readMemory(stringValue(session, "canonical_path")); err != nil {
			fmt.Fprintf(os.Stderr, "myriad: native %s is invalid and was left in place: %v\n", MemoryName, err)
		}
		return
	}
	localPath, canonicalPath := stringValue(session, "local_path"), stringValue(session, "canonical_path")
	proposed, err := readMemory(localPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "myriad: native %s update preserved in %s: %v\n", MemoryName, localPath, err)
		return
	}
	base := recordMap(session, "base")
	common, _ := gitCommonDir(filepath.Dir(localPath))
	lock, err := store.Lock("memory:"+common, true)
	if err != nil {
		return
	}
	defer lock.Unlock()
	current, err := readMemory(canonicalPath)
	if err != nil {
		return
	}
	overwrites := []string{}
	merged := mergeMemory(base, current, proposed, "", &overwrites).(Record)
	if writeMemory(canonicalPath, merged) == nil {
		_ = writeMemory(localPath, merged)
	}
	if len(overwrites) > 0 {
		fmt.Fprintf(os.Stderr, "myriad: native %s merge overwrote concurrent fields: %s\n", MemoryName, strings.Join(overwrites, ", "))
	}
}

func printTaskList(store *Store) {
	tasks := store.All(true)
	sortTasksNewest(tasks)
	if len(tasks) == 0 {
		fmt.Println("No Myriad tasks.")
		return
	}
	fmt.Printf("%-31s %-21s %-8s %-16s %-10s %s\n", "TASK", "STATUS", "AGENT", "TARGET", "RESULT", "NOTE")
	for _, task := range tasks {
		notes := []string{}
		if !boolValue(task, "auto_integrate", true) && stringValue(task, "status") == StatusReady {
			notes = append(notes, "manual-integration")
		}
		if stringValue(task, "memory_warning") != "" || task["memory_overwrites"] != nil || task["memory_update"] != nil {
			notes = append(notes, "memory")
		}
		note := "-"
		if len(notes) > 0 {
			note = strings.Join(notes, ",")
		}
		fmt.Printf("%-31.31s %-21.21s %-8.8s %-16.16s %-10.10s %s\n", stringValue(task, "task_id"), stringValue(task, "status"), stringValue(task, "agent"), firstNonempty(stringValue(task, "target_branch"), "-"), firstNonempty(stringValue(task, "result_commit"), "-"), note)
	}
}
