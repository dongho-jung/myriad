package myriad

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/shlex"
	"golang.org/x/sys/unix"
)

func runValidationProcess(command []string, directory string, environment []string, timeout time.Duration, started func(Record)) (error, bool) {
	executable, err := executablePath()
	if err != nil {
		return err, false
	}
	arguments := append([]string{internalValidate, "--"}, command...)
	cmd := exec.Command(executable, arguments...)
	cmd.Dir = directory
	cmd.Env = environment
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: unix.SIGTERM}
	if err := cmd.Start(); err != nil {
		return err, false
	}
	if started != nil {
		started(processRecord(cmd.Process.Pid, "validation", cmd.Process.Pid))
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case waitErr := <-done:
		return waitErr, false
	case <-timer.C:
		_ = unix.Kill(cmd.Process.Pid, unix.SIGTERM)
	}
	grace := time.NewTimer(5 * time.Second)
	defer grace.Stop()
	select {
	case waitErr := <-done:
		return waitErr, true
	case <-grace.C:
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		return <-done, true
	}
}

func removeIntegrationWorktree(repository, path string) bool {
	_, _ = gitCommand(repository, false, "worktree", "remove", "--force", path)
	if _, err := os.Stat(path); err == nil {
		return false
	}
	_, _ = gitCommand(repository, false, "worktree", "prune")
	records, err := listedWorktrees(repository)
	if err != nil {
		return false
	}
	expected, _ := canonical(path)
	for _, record := range records {
		candidate, _ := canonical(record["worktree"])
		if candidate == expected {
			return false
		}
	}
	return true
}

func validationCommands(task Record, candidate, targetSHA string) ([][]string, []string, error) {
	commands := [][]string{}
	directories := []string{}
	candidateTask := cloneRecord(task)
	candidateTask["worktree_path"] = candidate
	workingDirectory := taskWorkingDirectory(candidateTask)
	for _, raw := range recordSlice(task, "checks") {
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		arguments, err := shlex.Split(value)
		if err != nil || len(arguments) == 0 {
			return nil, nil, fail("invalid validation command %q: %v", value, err)
		}
		commands = append(commands, arguments)
		directories = append(directories, workingDirectory)
	}
	changed, err := gitCommand(candidate, true, "diff", "--name-only", targetSHA+"..HEAD")
	if err != nil {
		return nil, nil, err
	}
	terraformChanged := false
	for _, path := range strings.Fields(changed.Stdout) {
		if strings.HasSuffix(path, ".tf") {
			terraformChanged = true
			break
		}
	}
	if terraformChanged {
		if _, err := exec.LookPath("terraform"); err == nil {
			commands = append(commands, []string{"terraform", "fmt", "-check", "-recursive", "."})
			directories = append(directories, candidate)
		}
	}
	return commands, directories, nil
}

func candidateUnchanged(candidate, expectedHead string) (bool, string) {
	current, err := gitRef(candidate, "HEAD")
	if err != nil || current != expectedHead {
		return false, "validation changed candidate HEAD"
	}
	changes, err := worktreeChanges(candidate)
	if err != nil {
		return false, err.Error()
	}
	if len(changes.Normal) > 0 {
		return false, fmt.Sprintf("validation changed candidate files: %v", changes.Normal[:min(20, len(changes.Normal))])
	}
	return true, ""
}

func validateCandidate(store *Store, task Record, candidate, targetSHA, expectedHead string) bool {
	if unchanged, reason := candidateUnchanged(candidate, expectedHead); !unchanged {
		task["validation_failure"] = Record{"reason": reason}
		if store != nil {
			_ = store.Save(task)
		}
		return false
	}
	commands, directories, err := validationCommands(task, candidate, targetSHA)
	if err != nil {
		task["validation_failure"] = Record{"reason": err.Error()}
		if store != nil {
			_ = store.Save(task)
		}
		return false
	}
	candidateTask := cloneRecord(task)
	candidateTask["worktree_path"] = candidate
	for index, command := range commands {
		fmt.Printf("validate: %s\n", displayCommand(command))
		timeoutSeconds := defaultCheckTimeout.Seconds()
		if raw, ok := task["check_timeout_seconds"].(json.Number); ok {
			if parsed, err := raw.Float64(); err == nil && parsed > 0 {
				timeoutSeconds = parsed
			}
		} else if parsed, ok := task["check_timeout_seconds"].(float64); ok && parsed > 0 {
			timeoutSeconds = parsed
		}
		timeout, ok := durationFromSeconds(timeoutSeconds)
		if !ok {
			timeout = defaultCheckTimeout
			timeoutSeconds = timeout.Seconds()
		}
		waitErr, timedOut := runValidationProcess(
			command, directories[index], taskEnvironment(candidateTask),
			timeout,
			func(owner Record) {
				if owner != nil {
					task["validation_process"] = owner
					if store != nil {
						_ = store.Save(task)
					}
				}
			},
		)
		if waitErr != nil && exitCode(waitErr) == 127 && !timedOut {
			task["validation_failure"] = Record{"command": stringsToAny(command), "reason": waitErr.Error()}
			if store != nil {
				_ = store.Save(task)
			}
			return false
		}
		delete(task, "validation_process")
		if store != nil {
			_ = store.Save(task)
		}
		if unchanged, reason := candidateUnchanged(candidate, expectedHead); !unchanged {
			task["validation_failure"] = Record{"command": stringsToAny(command), "exit_code": exitCode(waitErr), "reason": reason}
			if store != nil {
				_ = store.Save(task)
			}
			return false
		}
		if timedOut {
			task["validation_failure"] = Record{"command": stringsToAny(command), "exit_code": 124, "reason": fmt.Sprintf("validation exceeded %g seconds", timeoutSeconds)}
			if store != nil {
				_ = store.Save(task)
			}
			return false
		}
		if waitErr != nil {
			task["validation_failure"] = Record{"command": stringsToAny(command), "exit_code": exitCode(waitErr)}
			if store != nil {
				_ = store.Save(task)
			}
			return false
		}
	}
	delete(task, "validation_failure")
	if store != nil {
		_ = store.Save(task)
	}
	return true
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 127
}

func terminateOwnedProcess(value any) {
	if !processAlive(value) {
		return
	}
	process := anyRecord(value)
	pid, ok := intValue(process["pid"])
	if !ok {
		return
	}
	pgid, ok := intValue(process["pgid"])
	if !ok {
		pgid = pid
	}
	actual, err := unix.Getpgid(pid)
	if err != nil || actual != pgid || pgid != pid {
		return
	}
	_ = unix.Kill(-pgid, unix.SIGTERM)
	deadline := time.Now().Add(time.Second)
	for processAlive(process) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(process) {
		_ = unix.Kill(-pgid, unix.SIGKILL)
	}
}

func rebaseIntegrationResult(path, targetSHA, base string) (string, bool, error) {
	rebased, err := gitCommand(
		path, false,
		"-c", "core.hooksPath=/dev/null",
		"-c", "commit.gpgSign=false",
		"-c", "rebase.updateRefs=false",
		"-c", "rebase.autoStash=false",
		"-c", "rerere.enabled=false",
		"-c", "notes.rewrite.rebase=false",
		"rebase", "--merge", "--rebase-merges", "--no-autostash",
		"--committer-date-is-author-date", "--empty=drop", "--keep-empty",
		"--onto", targetSHA, base,
	)
	if err != nil {
		return "", false, err
	}
	if rebased.ExitCode != 0 {
		conflicts, _ := gitCommand(path, false, "diff", "--name-only", "--diff-filter=U")
		if strings.TrimSpace(conflicts.Stdout) != "" {
			return "", true, nil
		}
		detail := strings.TrimSpace(rebased.Stderr + rebased.Stdout)
		if detail == "" {
			detail = fmt.Sprintf("git rebase exited with %d", rebased.ExitCode)
		}
		return "", false, fail("integration rebase failed: %s", detail)
	}
	head, err := gitRef(path, "HEAD")
	return head, false, err
}

func createIntegrationCandidate(repository string, task Record, targetSHA, resultCommit, candidate string) (string, string, error) {
	if err := os.MkdirAll(filepath.Dir(candidate), 0o700); err != nil {
		return "", "", err
	}
	if _, err := gitCommand(repository, true, "-c", "core.hooksPath=/dev/null", "worktree", "add", "--detach", candidate, resultCommit); err != nil {
		return "", "", err
	}
	if isAncestor(repository, targetSHA, resultCommit) {
		return resultCommit, "fast-forward", nil
	}
	base := stringValue(task, "base_sha")
	if base == "" {
		return "", "", fail("integration candidate has no recorded task base")
	}
	head, conflicted, err := rebaseIntegrationResult(candidate, targetSHA, base)
	if err != nil {
		return "", "", err
	}
	if conflicted {
		return "", "rebase", nil
	}
	return head, "rebase", nil
}

func synchronizePublishedRebase(path, base, targetSHA, resultCommit, candidateHead string) error {
	current, err := gitRef(path, "HEAD")
	if err != nil || current != resultCommit {
		return fail("task worktree changed while publish was synchronizing")
	}
	changes, err := worktreeChanges(path)
	if err != nil || len(changes.Normal) > 0 {
		return fail("task worktree changed while publish was synchronizing")
	}
	head, conflicted, err := rebaseIntegrationResult(path, targetSHA, base)
	if err != nil || conflicted {
		_, _ = gitCommand(path, false, "-c", "core.hooksPath=/dev/null", "rebase", "--abort")
		if err != nil {
			return err
		}
		return fail("task worktree conflicted while synchronizing the published rebase")
	}
	if unchanged, reason := candidateUnchanged(path, candidateHead); head != candidateHead || !unchanged {
		return fail("task worktree changed while publish was synchronizing: %s", firstNonempty(reason, "rebased HEAD differs from validated candidate"))
	}
	return nil
}

func advanceIntegrationTarget(repository, target, targetSHA, candidateHead, initialCheckout string) string {
	currentSHA, err := gitRef(repository, "refs/heads/"+target)
	if err != nil || currentSHA != targetSHA {
		return "target advanced during validation"
	}
	checkout, err := targetCheckout(repository, target)
	if err != nil {
		return err.Error()
	}
	initial, _ := canonical(initialCheckout)
	final, _ := canonical(checkout)
	if initial != final {
		return "target checkout topology changed during validation"
	}
	if checkout != "" {
		changes, err := worktreeChanges(checkout)
		if err != nil {
			return "target checkout could not be inspected: " + err.Error()
		}
		if len(changes.Normal) > 0 {
			return "target checkout became dirty: " + checkout
		}
		advanced, err := gitCommand(checkout, false, "-c", "core.hooksPath=/dev/null", "merge", "--ff-only", candidateHead)
		if err != nil || advanced.ExitCode != 0 {
			return "target could not fast-forward"
		}
		return ""
	}
	updated, err := gitCommand(repository, false, "update-ref", "refs/heads/"+target, candidateHead, targetSHA)
	if err != nil || updated.ExitCode != 0 {
		return "target advanced"
	}
	return ""
}

func deferIntegration(store *Store, task Record, status, reason string, repositoryReserved bool) bool {
	_ = setStatus(store, task, status, reason)
	_, _ = cleanupTask(store, task, repositoryReserved, false)
	return false
}

func queuedReason(store *Store, repository string, task Record, reason string) string {
	if count := notifyActiveSessions(store, repository, task); count > 0 {
		return fmt.Sprintf("%s; handoff requested from %d active session(s)", reason, count)
	}
	return reason
}

func integrateTask(store *Store, task Record) bool {
	repository := stringValue(task, "repository")
	target := stringValue(task, "target_branch")
	resultCommit := stringValue(task, "result_commit")
	if target == "" || resultCommit == "" {
		return deferIntegration(store, task, StatusRecovery, "integration metadata is incomplete", false)
	}
	lockName := "integrate:" + stringValue(task, "git_common_dir") + ":" + target
	integrationLock, err := store.Lock(lockName, false)
	if err != nil {
		if isLockBusy(err) {
			return deferIntegration(store, task, StatusReady, "another integration is running; integration queued", false)
		}
		return deferIntegration(store, task, StatusRecovery, err.Error(), false)
	}
	defer integrationLock.Unlock()
	if branchExists(repository, target) {
		targetSHA, err := gitRef(repository, "refs/heads/"+target)
		if err != nil {
			return deferIntegration(store, task, StatusRecovery, "cannot resolve target branch: "+err.Error(), false)
		}
		if isAncestor(repository, resultCommit, targetSHA) {
			task["integrated_commit"] = targetSHA
			task["integration_strategy"] = "already-present"
			delete(task, "integration_redundant_result")
			_ = setStatus(store, task, StatusIntegrated, "")
			resolveTaskNotices(store, stringValue(task, "task_id"))
			_ = applyMemoryUpdate(store, task)
			_, _ = cleanupTask(store, task, false, false)
			return true
		}
	}
	activity, err := store.RepositoryActivityLock(repository, true, false)
	if err != nil {
		if isLockBusy(err) {
			return deferIntegration(store, task, StatusReady, queuedReason(store, repository, task, "repository has an active agent; integration queued"), false)
		}
		return deferIntegration(store, task, StatusRecovery, err.Error(), false)
	}
	defer activity.Unlock()
	key, _ := repoKey(repository)
	candidate := filepath.Join(store.Integrations, key, stringValue(task, "task_id"))
	if !removeIntegrationWorktree(repository, candidate) {
		return deferIntegration(store, task, StatusRecovery, "stale integration worktree could not be removed: "+candidate, true)
	}
	checkoutLocks := []*fileLock{}
	paths, err := sortedCheckoutPaths(repository)
	if err != nil {
		return deferIntegration(store, task, StatusRecovery, err.Error(), true)
	}
	for _, path := range paths {
		lock, lockErr := store.CheckoutLock(path, "", false)
		if lockErr != nil {
			for _, held := range checkoutLocks {
				_ = held.Unlock()
			}
			return deferIntegration(store, task, StatusReady, queuedReason(store, repository, task, "checkout has an active agent; integration queued: "+path), true)
		}
		checkoutLocks = append(checkoutLocks, lock)
	}
	defer func() {
		for _, lock := range checkoutLocks {
			_ = lock.Unlock()
		}
	}()
	return integrateTaskReserved(store, task, repository, target, resultCommit, candidate)
}

func integrateTaskReserved(store *Store, task Record, repository, target, resultCommit, candidate string) bool {
	targetSHA, err := gitRef(repository, "refs/heads/"+target)
	if err != nil {
		return deferIntegration(store, task, StatusRecovery, "cannot resolve target branch "+target+": "+err.Error(), true)
	}
	base := stringValue(task, "base_sha")
	if base == "" || !isAncestor(repository, base, resultCommit) {
		return deferIntegration(store, task, StatusRecovery, "result does not descend from the recorded base", true)
	}
	findings, err := forbiddenHistory(repository, resultCommit, targetSHA)
	if err != nil {
		return deferIntegration(store, task, StatusRecovery, err.Error(), true)
	}
	if len(findings) > 0 {
		task["forbidden_history"] = recordsToAny(findings)
		_ = store.Save(task)
		return deferIntegration(store, task, StatusRecovery, "result history tracks forbidden paths: "+strings.Join(findingPaths(findings), ", "), true)
	}
	if isAncestor(repository, resultCommit, targetSHA) {
		task["integrated_commit"] = targetSHA
		_ = setStatus(store, task, StatusIntegrated, "result was already present on target")
		resolveTaskNotices(store, stringValue(task, "task_id"))
		_ = applyMemoryUpdate(store, task)
		_, _ = cleanupTask(store, task, true, false)
		return true
	}
	if !isAncestor(repository, base, targetSHA) {
		return deferIntegration(store, task, StatusRecovery, "target no longer descends from the task base; automatic integration refused", true)
	}
	checkout, err := targetCheckout(repository, target)
	if err != nil {
		return deferIntegration(store, task, StatusRecovery, "cannot inspect target checkout topology: "+err.Error(), true)
	}
	if checkout != "" {
		changes, err := worktreeChanges(checkout)
		if err != nil {
			return deferIntegration(store, task, StatusReady, "target checkout could not be inspected; integration queued: "+checkout, true)
		}
		if len(changes.Normal) > 0 {
			return deferIntegration(store, task, StatusReady, "target checkout is dirty; integration queued: "+checkout, true)
		}
	}
	owner := processRecord(os.Getpid(), "integration", 0)
	if owner == nil {
		return deferIntegration(store, task, StatusRecovery, "cannot record integration process identity", true)
	}
	task["integration_process"] = owner
	task["integration_candidate"] = candidate
	_ = setStatus(store, task, StatusIntegrating, "")
	defer func() {
		terminateOwnedProcess(task["validation_process"])
		if removeIntegrationWorktree(repository, candidate) {
			delete(task, "integration_cleanup_warning")
		} else {
			task["integration_cleanup_warning"] = "integration worktree cleanup failed: " + candidate
		}
		delete(task, "validation_process")
		delete(task, "integration_process")
		delete(task, "integration_candidate")
		_ = store.Save(task)
	}()
	candidateHead, strategy, err := createIntegrationCandidate(repository, task, targetSHA, resultCommit, candidate)
	if err != nil {
		return deferIntegration(store, task, StatusRecovery, err.Error(), true)
	}
	if candidateHead == "" {
		return deferIntegration(store, task, StatusRecovery, "integration conflict; committed result preserved", true)
	}
	_ = setStatus(store, task, StatusValidating, "")
	if !validateCandidate(store, task, candidate, targetSHA, candidateHead) {
		return deferIntegration(store, task, StatusRecovery, "integration candidate failed validation", true)
	}
	if unchanged, reason := candidateUnchanged(candidate, candidateHead); !unchanged {
		task["validation_failure"] = Record{"reason": reason}
		_ = store.Save(task)
		return deferIntegration(store, task, StatusRecovery, "integration candidate changed after validation", true)
	}
	differs, err := treesDiffer(repository, targetSHA, candidateHead)
	if err != nil {
		return deferIntegration(store, task, StatusRecovery, "cannot compare integration candidate: "+err.Error(), true)
	}
	if !differs {
		task["integrated_commit"] = targetSHA
		task["integration_strategy"] = "redundant"
		task["integration_redundant_result"] = resultCommit
		_ = setStatus(store, task, StatusIntegrated, "result changes were already present on target")
		resolveTaskNotices(store, stringValue(task, "task_id"))
		_ = applyMemoryUpdate(store, task)
	} else {
		if reason := advanceIntegrationTarget(repository, target, targetSHA, candidateHead, checkout); reason != "" {
			return deferIntegration(store, task, StatusReady, reason+"; integration queued", true)
		}
		task["integrated_commit"] = candidateHead
		task["integration_strategy"] = strategy
		delete(task, "integration_redundant_result")
		_ = setStatus(store, task, StatusIntegrated, "")
		resolveTaskNotices(store, stringValue(task, "task_id"))
		_ = applyMemoryUpdate(store, task)
	}
	_, _ = cleanupTask(store, task, true, false)
	return stringValue(task, "integrated_commit") != ""
}

func finalizeTask(store *Store, task Record, integrate, trustCleanCommit bool) error {
	if err := inspectResult(store, task, trustCleanCommit); err != nil {
		return err
	}
	if stringValue(task, "status") != StatusReady {
		return nil
	}
	cleaned, err := cleanupTask(store, task, false, false)
	if err != nil {
		return err
	}
	if integrate && boolValue(task, "auto_integrate", true) && cleaned {
		integrateTask(store, task)
	}
	return nil
}

func publishTaskCheckpoint(store *Store, task Record) (Record, error) {
	if stringValue(task, "status") != StatusRunning || !processAlive(task["process"]) {
		return nil, fail("task is not actively running: %s", stringValue(task, "task_id"))
	}
	path, err := managedWorktreePath(store, task)
	if err != nil {
		return nil, err
	}
	changes, err := worktreeChanges(path)
	if err != nil {
		return nil, err
	}
	if len(changes.Normal) > 0 {
		return nil, fail("publish requires a clean committed worktree: %v", changes.Normal[:min(20, len(changes.Normal))])
	}
	branch, _ := gitCommand(path, true, "branch", "--show-current")
	if strings.TrimSpace(branch.Stdout) != stringValue(task, "branch") {
		return nil, fail("unexpected task branch: %s", strings.TrimSpace(branch.Stdout))
	}
	resultCommit, err := gitRef(path, "HEAD")
	if err != nil {
		return nil, err
	}
	base := stringValue(task, "base_sha")
	repository := stringValue(task, "repository")
	if base == "" || !isAncestor(repository, base, resultCommit) {
		return nil, fail("publish result does not descend from the recorded task base")
	}
	target := stringValue(task, "target_branch")
	if target == "" || !branchExists(repository, target) {
		return nil, fail("publish target branch is unavailable: %s", firstNonempty(target, "(missing)"))
	}
	key, _ := repoKey(repository)
	candidate := filepath.Join(store.Integrations, key, stringValue(task, "task_id")+"-publish")
	defer removeIntegrationWorktree(repository, candidate)
	publishLock, err := store.Lock("publish:"+stringValue(task, "task_id"), false)
	if err != nil {
		return nil, fail("another publish or integration is running")
	}
	defer publishLock.Unlock()
	integrationLock, err := store.Lock("integrate:"+stringValue(task, "git_common_dir")+":"+target, false)
	if err != nil {
		return nil, fail("another publish or integration is running")
	}
	defer integrationLock.Unlock()
	activity, err := store.RepositoryActivityLock(repository, false, false)
	if err != nil {
		return nil, fail("repository has another active lifecycle operation")
	}
	defer activity.Unlock()
	targetSHA, err := gitRef(repository, "refs/heads/"+target)
	if err != nil {
		return nil, err
	}
	if !isAncestor(repository, base, targetSHA) {
		return nil, fail("target no longer descends from the recorded task base")
	}
	if isAncestor(repository, resultCommit, targetSHA) {
		return Record{"result_commit": resultCommit, "published_commit": targetSHA, "strategy": "already-present"}, nil
	}
	findings, err := forbiddenHistory(repository, resultCommit, targetSHA)
	if err != nil {
		return nil, err
	}
	if len(findings) > 0 {
		return nil, fail("publish result tracks forbidden paths: %s", strings.Join(findingPaths(findings), ", "))
	}
	if !removeIntegrationWorktree(repository, candidate) {
		return nil, fail("stale publish candidate could not be removed: %s", candidate)
	}
	checkout, err := targetCheckout(repository, target)
	if err != nil {
		return nil, err
	}
	var checkoutLock *fileLock
	if checkout != "" && checkout != path {
		checkoutLock, err = store.CheckoutLock(checkout, "", false)
		if err != nil {
			return nil, fail("target checkout has an active agent: %s", checkout)
		}
		defer checkoutLock.Unlock()
	}
	if checkout != "" {
		changes, err := worktreeChanges(checkout)
		if err != nil {
			return nil, err
		}
		if len(changes.Normal) > 0 {
			return nil, fail("target checkout is dirty: %s", checkout)
		}
	}
	candidateHead, strategy, err := createIntegrationCandidate(repository, task, targetSHA, resultCommit, candidate)
	if err != nil || candidateHead == "" {
		return nil, fail("publish candidate conflicts with the current target")
	}
	validationTask := cloneRecord(task)
	if !validateCandidate(nil, validationTask, candidate, targetSHA, candidateHead) {
		return nil, fail("publish candidate failed validation: %s", describe(validationTask["validation_failure"]))
	}
	if unchanged, reason := candidateUnchanged(candidate, candidateHead); !unchanged {
		return nil, fail("publish candidate changed after validation: %s", reason)
	}
	current, err := gitRef(path, "HEAD")
	if err != nil {
		return nil, err
	}
	currentChanges, err := worktreeChanges(path)
	if err != nil {
		return nil, err
	}
	if current != resultCommit || len(currentChanges.Normal) > 0 {
		return nil, fail("task worktree changed while publish was validating")
	}
	if strategy == "rebase" {
		if err := synchronizePublishedRebase(path, base, targetSHA, resultCommit, candidateHead); err != nil {
			return nil, err
		}
	}
	differs, err := treesDiffer(repository, targetSHA, candidateHead)
	if err != nil {
		return nil, err
	}
	if !differs {
		return Record{"result_commit": resultCommit, "published_commit": targetSHA, "strategy": "redundant"}, nil
	}
	if reason := advanceIntegrationTarget(repository, target, targetSHA, candidateHead, checkout); reason != "" {
		return nil, fail("publish target changed: %s", reason)
	}
	return Record{"result_commit": resultCommit, "published_commit": candidateHead, "strategy": strategy}, nil
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
