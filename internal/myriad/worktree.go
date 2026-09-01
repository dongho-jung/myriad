package myriad

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type createTaskOptions struct {
	LaunchCWD    string
	Agent        string
	Target       string
	Checks       []string
	CheckTimeout float64
	NoIntegrate  bool
	Description  string
	TaskSlug     string
	Quiet        bool
	Deferred     bool
}

func taskIDAvailable(store *Store, repositoryKey, taskID string) (bool, error) {
	registry, err := store.TaskPath(taskID)
	if err != nil {
		return false, err
	}
	for _, path := range []string{registry, filepath.Join(store.Worktrees, repositoryKey, taskID)} {
		if _, err := os.Lstat(path); err == nil {
			return false, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return true, nil
}

func createTask(store *Store, options createTaskOptions) (Record, error) {
	cwd, _ := canonical(options.LaunchCWD)
	checkout, err := repoRoot(cwd)
	if err != nil {
		return nil, err
	}
	current, err := gitCommand(checkout, true, "branch", "--show-current")
	if err != nil {
		return nil, err
	}
	currentBranch := strings.TrimSpace(current.Stdout)
	repository, err := primaryWorktree(checkout)
	if err != nil {
		return nil, err
	}
	changes, err := worktreeChanges(checkout)
	if err != nil {
		return nil, err
	}
	if len(changes.Normal) > 0 && !options.Quiet {
		fmt.Printf("current checkout changes stay in place and are not inherited: %s\n", checkout)
	}
	memoryPath, memory, err := ensureMemory(store, repository, currentBranch)
	if err != nil {
		return nil, err
	}
	target := options.Target
	if target == "" {
		target = stringValue(recordMap(memory, "settings"), "integration_target")
	}
	if target == "" {
		return nil, fail("no target branch; set --target or settings.integration_target in %s", memoryPath)
	}
	if !branchExists(repository, target) {
		return nil, fail("target branch from %s does not exist locally: %s", memoryPath, target)
	}
	base, err := gitRef(repository, "refs/heads/"+target)
	if err != nil {
		return nil, err
	}
	tracked, err := commitTracksForbiddenPaths(repository, base)
	if err != nil {
		return nil, err
	}
	if len(tracked) > 0 {
		return nil, fail("machine-local paths are tracked on target branch %s: %s", target, strings.Join(tracked, ", "))
	}
	taskID, err := formatTaskID()
	if err != nil {
		return nil, err
	}
	repositoryKey, err := repoKey(repository)
	if err != nil {
		return nil, err
	}
	worktree := filepath.Join(store.Worktrees, repositoryKey, taskID)
	relative, err := filepath.Rel(checkout, cwd)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		relative = "."
	}
	owner := processRecord(os.Getpid(), "launcher", 0)
	if owner == nil {
		return nil, fail("cannot record task launcher process identity")
	}
	timeout := options.CheckTimeout
	if timeout <= 0 {
		timeout = defaultCheckTimeout.Seconds()
	}
	common, err := gitCommonDir(repository)
	if err != nil {
		return nil, err
	}
	task := Record{
		"schema_version":           TaskRecordSchema,
		"task_id":                  taskID,
		"repository":               repository,
		"git_common_dir":           common,
		"base_sha":                 base,
		"base_source":              "integration_target",
		"source_branch":            target,
		"target_branch":            target,
		"branch":                   nil,
		"worktree_path":            worktree,
		"workdir_relative":         relative,
		"origin_working_directory": cwd,
		"memory_path":              memoryPath,
		"memory_base":              cloneRecord(memory),
		"memory_pending":           false,
		"agent":                    options.Agent,
		"resume_hint":              resumeHint(options.Agent),
		"description":              options.Description,
		"checks":                   stringsToAny(options.Checks),
		"check_timeout_seconds":    timeout,
		"auto_integrate":           !options.NoIntegrate,
		"worktree_state":           worktreeCreating,
		"status":                   StatusCreated,
		"created_at":               now(),
		"process":                  owner,
	}
	if options.Deferred {
		task["worktree_state"] = worktreePending
	}
	slug := options.TaskSlug
	if slug == "" && !options.Deferred {
		slug = fallbackTaskSlug(firstNonempty(options.Description, options.Agent+"-task"))
	}
	if slug != "" {
		validated, err := taskSlug(slug)
		if err != nil {
			return nil, err
		}
		task["provisioning_slug"] = validated
		task["title"] = validated
	}
	numberLock, err := store.Lock("worktree-number", true)
	if err != nil {
		return nil, err
	}
	for attempts := 0; ; attempts++ {
		available, checkErr := taskIDAvailable(store, repositoryKey, taskID)
		if checkErr != nil {
			_ = numberLock.Unlock()
			return nil, checkErr
		}
		if available {
			break
		}
		if attempts == 99 {
			_ = numberLock.Unlock()
			return nil, fail("cannot allocate a unique task id")
		}
		taskID, err = formatTaskID()
		if err != nil {
			_ = numberLock.Unlock()
			return nil, err
		}
		worktree = filepath.Join(store.Worktrees, repositoryKey, taskID)
		task["task_id"] = taskID
		task["worktree_path"] = worktree
	}
	task["worktree_number"] = nextWorktreeNumber(store.All(false))
	if err := store.Save(task); err != nil {
		_ = numberLock.Unlock()
		return nil, err
	}
	_ = numberLock.Unlock()
	if err := os.MkdirAll(filepath.Dir(worktree), 0o700); err != nil {
		return nil, err
	}
	if options.Deferred {
		if err := os.Mkdir(worktree, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			_ = setStatus(store, task, StatusFailed, err.Error())
			return nil, err
		}
		if err := store.Save(task); err != nil {
			return nil, err
		}
		return task, nil
	}
	if _, err := provisionTaskWorktree(store, task, slug); err != nil {
		_ = setStatus(store, task, StatusFailed, err.Error())
		return nil, err
	}
	return task, nil
}

func stringsToAny(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}

func provisionTaskWorktree(store *Store, task Record, slug string) (string, error) {
	repository := stringValue(task, "repository")
	common, err := gitCommonDir(repository)
	if err != nil {
		return "", err
	}
	lock, err := store.Lock("branch-allocation:"+common, true)
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Unlock() }()
	return provisionTaskWorktreeReserved(store, task, slug)
}

func provisionTaskWorktreeReserved(store *Store, task Record, slug string) (string, error) {
	selected := stringValue(task, "provisioning_slug")
	if selected == "" {
		selected = slug
	}
	validated, err := taskSlug(selected)
	if err != nil {
		return "", err
	}
	if taskWorktreeReady(task) {
		branch := stringValue(task, "branch")
		if branch == "" {
			return "", fail("ready task has no branch")
		}
		return branch, nil
	}
	path, err := managedWorktreePath(store, task)
	if err != nil {
		return "", err
	}
	repository := stringValue(task, "repository")
	base := stringValue(task, "base_sha")
	branch := stringValue(task, "branch")
	if branch == "" {
		branch = availableTaskBranch(repository, validated)
	}
	task["branch"] = branch
	task["provisioning_slug"] = validated
	task["worktree_state"] = worktreeCreating
	delete(task, "provisioning_error")
	if err := store.Save(task); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	probe, err := gitCommand(path, false, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return "", err
	}
	if probe.ExitCode == 0 {
		current, err := gitCommand(path, true, "branch", "--show-current")
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(current.Stdout) != branch {
			return "", fail("deferred worktree has unexpected branch %s", strings.TrimSpace(current.Stdout))
		}
	} else {
		entries, err := os.ReadDir(path)
		if err != nil {
			return "", err
		}
		if len(entries) > 0 {
			names := []string{}
			for _, entry := range entries {
				names = append(names, entry.Name())
				if len(names) == 10 {
					break
				}
			}
			return "", fail("deferred worktree path is not empty: %s (%s)", path, strings.Join(names, ", "))
		}
		arguments := []string{"-c", "core.hooksPath=/dev/null", "worktree", "add"}
		if branchExists(repository, branch) {
			head, _ := gitRef(repository, "refs/heads/"+branch)
			if head != base {
				return "", fail("deferred task branch moved before provisioning: %s", branch)
			}
			arguments = append(arguments, path, branch)
		} else {
			arguments = append(arguments, "-b", branch, path, base)
		}
		if _, err := gitCommand(repository, true, arguments...); err != nil {
			task["provisioning_error"] = err.Error()
			_ = store.Save(task)
			return "", err
		}
	}
	if _, err := gitCommand(repository, true, "worktree", "lock", "--reason", "myriad:"+stringValue(task, "task_id"), path); err != nil {
		return "", err
	}
	memory := recordMap(task, "memory_base")
	if _, err := os.Stat(filepath.Join(path, MemoryName)); errors.Is(err, os.ErrNotExist) {
		if err := stageMemory(task, memory); err != nil {
			return "", err
		}
	}
	task["worktree_state"] = worktreeReady
	task["provisioned_at"] = now()
	delete(task, "provisioning_error")
	if err := store.Save(task); err != nil {
		return "", err
	}
	return branch, nil
}

func recreateWorktree(store *Store, task Record) error {
	path, err := managedWorktreePath(store, task)
	if err != nil {
		return err
	}
	repository := stringValue(task, "repository")
	if !taskWorktreeReady(task) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		probe, err := gitCommand(path, false, "rev-parse", "--is-inside-work-tree")
		if err != nil {
			return err
		}
		if probe.ExitCode != 0 {
			return nil
		}
		current, _ := gitCommand(path, true, "branch", "--show-current")
		if strings.TrimSpace(current.Stdout) != stringValue(task, "branch") {
			return fail("partially provisioned worktree has unexpected branch %s", strings.TrimSpace(current.Stdout))
		}
		if _, err := os.Stat(filepath.Join(path, MemoryName)); errors.Is(err, os.ErrNotExist) {
			if err := stageMemory(task, recordMap(task, "memory_base")); err != nil {
				return err
			}
		}
		task["worktree_state"] = worktreeReady
		task["provisioned_at"] = firstNonempty(stringValue(task, "provisioned_at"), now())
		delete(task, "provisioning_error")
		return store.Save(task)
	}
	if _, err := os.Stat(path); err == nil {
		registered, inspectErr := worktreeRegistered(repository, path)
		if inspectErr != nil {
			return inspectErr
		}
		if registered {
			return nil
		}
		if _, err := quarantineUnregisteredWorktree(store, task, path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	branch := stringValue(task, "branch")
	if !branchExists(repository, branch) {
		start := firstNonempty(stringValue(task, "result_commit"), stringValue(task, "base_sha"))
		if _, err := gitCommand(repository, true, "branch", branch, start); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := gitCommand(repository, true, "-c", "core.hooksPath=/dev/null", "worktree", "add", path, branch); err != nil {
		return err
	}
	if _, err := gitCommand(repository, true, "worktree", "lock", "--reason", "myriad:"+stringValue(task, "task_id"), path); err != nil {
		return err
	}
	if stringValue(task, "memory_path") != "" {
		if update := recordMap(task, "memory_update"); update != nil {
			base := recordMap(update, "base")
			proposed := recordMap(update, "proposed")
			if err := writeMemory(filepath.Join(path, MemoryName), proposed); err != nil {
				return err
			}
			task["memory_base"] = base
			task["memory_pending"] = true
			delete(task, "memory_update")
		} else {
			memory, err := readMemory(stringValue(task, "memory_path"))
			if err != nil {
				return err
			}
			if err := stageMemory(task, memory); err != nil {
				return err
			}
		}
	}
	delete(task, "worktree_cleaned_at")
	task["worktree_recreated_at"] = now()
	return store.Save(task)
}

func quarantineUnregisteredWorktree(store *Store, task Record, path string) (string, error) {
	registered, err := worktreeRegistered(stringValue(task, "repository"), path)
	if err != nil {
		return "", err
	}
	if registered {
		return "", fail("refused to quarantine registered worktree: %s", path)
	}
	random, err := randomHex(4)
	if err != nil {
		return "", err
	}
	destination := filepath.Join(store.Quarantine, stringValue(task, "task_id")+"-"+random)
	if err := os.Rename(path, destination); err != nil {
		return "", err
	}
	records := recordSlice(task, "worktree_quarantines")
	records = append(records, Record{
		"moved_at": now(), "path": destination,
		"reason": "managed path existed but was not registered as a Git worktree",
	})
	task["worktree_quarantines"] = records
	return destination, store.Save(task)
}

func cleanupTask(store *Store, task Record, repositoryReserved, checkoutReserved bool) (bool, error) {
	path, err := managedWorktreePath(store, task)
	if err != nil {
		return false, err
	}
	if !taskWorktreeReady(task) {
		return cleanupTaskReserved(store, task)
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cleanupTaskReserved(store, task)
		}
		return false, err
	}
	repository := stringValue(task, "repository")
	var activity *fileLock
	if !repositoryReserved {
		activity, err = store.RepositoryActivityLock(repository, false, true)
		if err != nil {
			return false, err
		}
		defer func() { _ = activity.Unlock() }()
	}
	var checkout *fileLock
	if !checkoutReserved {
		identity, _ := taskCheckoutIdentity(task)
		checkout, err = store.CheckoutLock(path, identity, false)
		if err != nil {
			if isLockBusy(err) {
				task["cleanup_warning"] = "worktree has an active agent; cleanup queued: " + path
				_ = store.Save(task)
				return false, nil
			}
			return false, err
		}
		defer func() { _ = checkout.Unlock() }()
	}
	delete(task, "cleanup_warning")
	return cleanupTaskReserved(store, task)
}

func cleanupTaskReserved(store *Store, task Record) (bool, error) {
	path, err := managedWorktreePath(store, task)
	if err != nil {
		return false, err
	}
	info, statErr := os.Stat(path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return false, statErr
	}
	exists := statErr == nil
	changed := false
	if exists && info.IsDir() && !taskWorktreeReady(task) {
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			return false, readErr
		}
		if len(entries) > 0 {
			_ = setStatus(store, task, StatusRecovery, "cleanup preserved partially provisioned worktree in "+path)
			return false, nil
		}
		if err := os.Remove(path); err != nil {
			return false, err
		}
		task["worktree_cleaned_at"] = now()
		changed = true
		exists = false
	}
	repository := stringValue(task, "repository")
	if exists && taskWorktreeReady(task) {
		registered, err := worktreeRegistered(repository, path)
		if err != nil {
			return false, err
		}
		if !registered {
			destination, err := quarantineUnregisteredWorktree(store, task, path)
			if err != nil {
				return false, err
			}
			changed = true
			exists = false
			status := stringValue(task, "status")
			if status != StatusIntegrated && status != StatusCompleted && status != StatusFailed {
				_ = setStatus(store, task, StatusRecovery, "unregistered worktree preserved in quarantine: "+destination)
				return false, nil
			}
		}
	}
	if exists {
		changes, err := worktreeChanges(path)
		if err != nil {
			return false, err
		}
		if len(changes.Normal) > 0 {
			_ = setStatus(store, task, StatusRecovery, "cleanup preserved uncommitted changes in "+path)
			return false, nil
		}
		if err := captureMemoryProposal(store, task); err != nil {
			return false, err
		}
		changes, err = worktreeChanges(path)
		if err != nil {
			return false, err
		}
		if len(changes.Normal) > 0 {
			_ = setStatus(store, task, StatusRecovery, "cleanup preserved uncommitted changes in "+path)
			return false, nil
		}
		if len(changes.Ignored) > 0 {
			sample := []any{}
			for index, line := range changes.Ignored {
				if index == 20 {
					break
				}
				sample = append(sample, strings.TrimSpace(strings.TrimPrefix(line, "!!")))
			}
			task["discarded_ignored_artifacts"] = Record{"count": len(changes.Ignored), "sample": sample}
			cleaned, err := gitCommand(path, false, "clean", "-ff", "-d", "-X")
			if err != nil {
				return false, err
			}
			if cleaned.ExitCode != 0 {
				_ = setStatus(store, task, StatusRecovery, "ignored artifact cleanup failed: "+strings.TrimSpace(cleaned.Stderr))
				return false, nil
			}
		}
		if _, err := gitCommand(repository, false, "worktree", "unlock", path); err != nil {
			return false, err
		}
		removed, err := gitCommand(repository, false, "worktree", "remove", path)
		if err != nil {
			return false, err
		}
		if removed.ExitCode != 0 {
			_, _ = gitCommand(repository, false, "worktree", "lock", "--reason", "myriad:"+stringValue(task, "task_id"), path)
			_ = setStatus(store, task, StatusRecovery, "worktree removal failed: "+strings.TrimSpace(removed.Stderr))
			return false, nil
		}
		task["worktree_cleaned_at"] = now()
		changed = true
	}
	if err := os.RemoveAll(filepath.Join(store.Scratch, stringValue(task, "task_id"))); err != nil {
		return false, err
	}
	if stringValue(task, "integrated_commit") != "" || stringValue(task, "status") == StatusCompleted {
		if err := applyMemoryUpdate(store, task); err != nil {
			return false, err
		}
	}
	branch := stringValue(task, "branch")
	if branch != "" && branchExists(repository, branch) {
		head, err := gitRef(repository, "refs/heads/"+branch)
		if err != nil {
			return false, err
		}
		safe := stringValue(task, "status") == StatusFailed && head == stringValue(task, "base_sha")
		if !safe && stringValue(task, "status") == StatusCompleted {
			differs, err := treesDiffer(repository, stringValue(task, "base_sha"), head)
			if err != nil {
				return false, err
			}
			safe = !differs
		}
		integrated := stringValue(task, "integrated_commit")
		if !safe && integrated != "" {
			safe = isAncestor(repository, head, integrated) || stringValue(task, "integration_redundant_result") == head
		}
		if safe {
			deleted, err := gitCommand(repository, false, "branch", "-D", branch)
			if err != nil {
				return false, err
			}
			if deleted.ExitCode != 0 {
				detail := strings.TrimSpace(deleted.Stderr + deleted.Stdout)
				if detail == "" {
					detail = fmt.Sprintf("git branch -D exited with %d", deleted.ExitCode)
				}
				task["cleanup_warning"] = "task branch deletion failed: " + detail
				return false, store.Save(task)
			}
			delete(task, "cleanup_warning")
			task["branch_deleted_at"] = now()
			changed = true
		}
	}
	if stringValue(task, "integrated_commit") != "" && !branchExists(repository, branch) && (stringValue(task, "status") != StatusIntegrated || stringValue(task, "status_reason") != "") {
		task["status"] = StatusIntegrated
		delete(task, "status_reason")
		changed = true
	}
	if changed {
		if err := store.Save(task); err != nil {
			return false, err
		}
	}
	return true, nil
}

func inspectResult(store *Store, task Record, trustCleanCommit bool) error {
	path, err := managedWorktreePath(store, task)
	if err != nil {
		return err
	}
	repository := stringValue(task, "repository")
	if !taskWorktreeReady(task) {
		if agentExitFailed(task) {
			return setStatus(store, task, StatusRecovery, fmt.Sprintf("agent exited with %d before worktree provisioning; session preserved", taskExitCode(task)))
		}
		if err := setStatus(store, task, StatusCompleted, "agent completed before repository work began"); err != nil {
			return err
		}
		_, err := cleanupTask(store, task, false, false)
		return err
	}
	_, statErr := os.Stat(path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	worktreeExists := statErr == nil
	if worktreeExists {
		changes, err := worktreeChanges(path)
		if err != nil {
			return err
		}
		if len(changes.Normal) > 0 {
			return setStatus(store, task, StatusRecovery, "uncommitted work preserved in "+path)
		}
		branch, err := gitCommand(path, true, "branch", "--show-current")
		if err != nil {
			return err
		}
		if strings.TrimSpace(branch.Stdout) != stringValue(task, "branch") {
			return setStatus(store, task, StatusRecovery, "unexpected branch preserved: "+strings.TrimSpace(branch.Stdout))
		}
	}
	head := currentHead(task)
	if _, err := adoptPreparedReplay(task, head); err != nil {
		return setStatus(store, task, StatusRecovery, "prepared replay result is invalid: "+err.Error())
	}
	if head == "" || head == stringValue(task, "base_sha") {
		if worktreeExists {
			if err := captureMemoryProposal(store, task); err != nil {
				return err
			}
		}
		if agentExitFailed(task) {
			return setStatus(store, task, StatusRecovery, fmt.Sprintf("agent exited with %d; session preserved", taskExitCode(task)))
		}
		if err := setStatus(store, task, StatusCompleted, "agent completed without repository changes"); err != nil {
			return err
		}
		if err := applyMemoryUpdate(store, task); err != nil {
			return err
		}
		_, err := cleanupTask(store, task, false, false)
		return err
	}
	if !isAncestor(repository, stringValue(task, "base_sha"), head) {
		return setStatus(store, task, StatusRecovery, "result does not descend from the recorded base")
	}
	task["result_commit"] = head
	target := stringValue(task, "target_branch")
	excluded := stringValue(task, "base_sha")
	if target != "" && branchExists(repository, target) {
		excluded, _ = gitRef(repository, "refs/heads/"+target)
	}
	findings, err := forbiddenHistory(repository, head, excluded)
	if err != nil {
		return err
	}
	if len(findings) > 0 {
		task["forbidden_history"] = recordsToAny(findings)
		task["memory_pending"] = false
		delete(task, "memory_update")
		task["memory_warning"] = "result history contains machine-local files; proposal was not captured"
		paths := findingPaths(findings)
		if err := setStatus(store, task, StatusRecovery, "result history tracks forbidden paths: "+strings.Join(paths, ", ")); err != nil {
			return err
		}
		_, err := cleanupTask(store, task, false, false)
		return err
	}
	delete(task, "forbidden_history")
	if worktreeExists {
		if err := captureMemoryProposal(store, task); err != nil {
			return err
		}
	}
	if agentExitFailed(task) && !trustCleanCommit {
		if err := setStatus(store, task, StatusRecovery, fmt.Sprintf("agent exited with %d; clean commit preserved", taskExitCode(task))); err != nil {
			return err
		}
		_, err := cleanupTask(store, task, false, false)
		return err
	}
	differs, err := treesDiffer(repository, stringValue(task, "base_sha"), head)
	if err != nil {
		return err
	}
	if !differs {
		reason := "agent completed without repository changes"
		if recordMap(task, "memory_update") != nil {
			reason += "; repository memory updated"
		}
		if err := setStatus(store, task, StatusCompleted, reason); err != nil {
			return err
		}
		if err := applyMemoryUpdate(store, task); err != nil {
			return err
		}
		_, err := cleanupTask(store, task, false, false)
		return err
	}
	return setStatus(store, task, StatusReady, "")
}

func recordsToAny(records []Record) []any {
	result := make([]any, len(records))
	for index, record := range records {
		result[index] = record
	}
	return result
}

func findingPaths(findings []Record) []string {
	seen := map[string]struct{}{}
	for _, finding := range findings {
		for _, raw := range recordSlice(finding, "paths") {
			if path, ok := raw.(string); ok {
				seen[path] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(seen))
	for path := range seen {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func taskExitCode(task Record) int {
	value, _ := intValue(task["agent_exit_code"])
	return value
}

func agentExitFailed(task Record) bool {
	exitCode := taskExitCode(task)
	return exitCode != 0 && (exitCode != 130 || !boolValue(task, "agent_exit_graceful", false))
}

func recordAgentExit(task Record, exitCode int, graceful bool) {
	task["agent_exit_code"] = exitCode
	if graceful {
		task["agent_exit_graceful"] = true
	} else {
		delete(task, "agent_exit_graceful")
	}
}

func resumeHint(agent string) string {
	if agent == "codex" {
		return "codex resume --last"
	}
	if agent == "claude" {
		return "claude --continue"
	}
	return ""
}
