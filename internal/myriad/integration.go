package myriad

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/shlex"
	"golang.org/x/sys/unix"
)

type tailWriter struct {
	mutex     sync.Mutex
	payload   []byte
	limit     int
	truncated bool
}

func newTailWriter(limit int) *tailWriter {
	return &tailWriter{limit: limit}
}

func (writer *tailWriter) Write(payload []byte) (int, error) {
	written := len(payload)
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	if writer.limit <= 0 {
		writer.truncated = writer.truncated || len(payload) > 0
		return written, nil
	}
	if len(payload) >= writer.limit {
		writer.payload = append(writer.payload[:0], payload[len(payload)-writer.limit:]...)
		writer.truncated = true
		return written, nil
	}
	if overflow := len(writer.payload) + len(payload) - writer.limit; overflow > 0 {
		copy(writer.payload, writer.payload[overflow:])
		writer.payload = writer.payload[:len(writer.payload)-overflow]
		writer.truncated = true
	}
	writer.payload = append(writer.payload, payload...)
	return written, nil
}

func (writer *tailWriter) snapshot() (string, bool) {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return string(writer.payload), writer.truncated
}

type validationProcessResult struct {
	WaitErr         error
	TimedOut        bool
	StdoutTail      string
	StderrTail      string
	OutputTruncated bool
}

func runValidationProcess(command []string, directory string, environment []string, timeout time.Duration, started func(Record) error) validationProcessResult {
	stdout, stderr := newTailWriter(validationTailBytes), newTailWriter(validationTailBytes)
	result := func(waitErr error, timedOut bool) validationProcessResult {
		stdoutTail, stdoutTruncated := stdout.snapshot()
		stderrTail, stderrTruncated := stderr.snapshot()
		return validationProcessResult{
			WaitErr: waitErr, TimedOut: timedOut,
			StdoutTail: stdoutTail, StderrTail: stderrTail,
			OutputTruncated: stdoutTruncated || stderrTruncated,
		}
	}
	executable, err := executablePath()
	if err != nil {
		return result(err, false)
	}
	arguments := append([]string{internalValidate, "--"}, command...)
	cmd := exec.Command(executable, arguments...)
	cmd.Dir = directory
	cmd.Env = overlayEnvironment(environment, map[string]string{
		"GIT_PAGER": "cat", "GIT_TERMINAL_PROMPT": "0", "PAGER": "cat",
	})
	// Validation is deliberately non-interactive. The foreground coding agent
	// has already exited when lifecycle checks run, so a validation child in its
	// own process group must never read from or write directly to the terminal.
	// Go attaches nil stdin to /dev/null; output is relayed by this foreground
	// launcher and retained as a bounded diagnostic tail.
	cmd.Stdin = nil
	cmd.Stdout = io.MultiWriter(stdout, os.Stdout)
	cmd.Stderr = io.MultiWriter(stderr, os.Stderr)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: unix.SIGTERM}
	if err := cmd.Start(); err != nil {
		return result(err, false)
	}
	if started != nil {
		if err := started(processRecord(cmd.Process.Pid, "validation", cmd.Process.Pid)); err != nil {
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
			_ = cmd.Wait()
			return result(err, false)
		}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case waitErr := <-done:
		return result(waitErr, false)
	case <-timer.C:
		_ = unix.Kill(cmd.Process.Pid, unix.SIGTERM)
	}
	grace := time.NewTimer(5 * time.Second)
	defer grace.Stop()
	select {
	case waitErr := <-done:
		return result(waitErr, true)
	case <-grace.C:
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		return result(<-done, true)
	}
}

func appendValidationAttempt(task Record, attempt Record) {
	history := append([]any{}, recordSlice(task, "validation_attempts")...)
	history = append(history, attempt)
	if len(history) > validationHistory {
		history = history[len(history)-validationHistory:]
	}
	task["validation_attempts"] = history
}

func recordValidationOutput(attempt Record, result validationProcessResult) {
	attempt["finished_at"] = now()
	attempt["exit_code"] = exitCode(result.WaitErr)
	attempt["timed_out"] = result.TimedOut
	if result.WaitErr == nil {
		attempt["outcome"] = "passed"
	} else if result.TimedOut {
		attempt["outcome"] = "timed_out"
	} else {
		attempt["outcome"] = "failed"
	}
	if result.StdoutTail != "" {
		attempt["stdout_tail"] = result.StdoutTail
	}
	if result.StderrTail != "" {
		attempt["stderr_tail"] = result.StderrTail
	}
	if result.OutputTruncated {
		attempt["output_truncated"] = true
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
	changed, err := gitCommand(candidate, true, "diff", "--name-only", "-z", targetSHA+"..HEAD")
	if err != nil {
		return nil, nil, err
	}
	terraformChanged := false
	for _, path := range strings.Split(changed.Stdout, "\x00") {
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

func saveValidationState(store *Store, task Record) error {
	if store == nil {
		return nil
	}
	return store.Save(task)
}

func validationFailed(store *Store, task Record, failure Record) (bool, error) {
	task["validation_failure"] = failure
	if err := saveValidationState(store, task); err != nil {
		return false, err
	}
	return false, nil
}

func validateCandidate(store *Store, task Record, candidate, targetSHA, expectedHead string) (bool, error) {
	if unchanged, reason := candidateUnchanged(candidate, expectedHead); !unchanged {
		return validationFailed(store, task, Record{"reason": reason})
	}
	commands, directories, err := validationCommands(task, candidate, targetSHA)
	if err != nil {
		return validationFailed(store, task, Record{"reason": err.Error()})
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
		attempt := Record{
			"command": stringsToAny(command), "directory": directories[index],
			"started_at": now(), "timeout_seconds": timeoutSeconds, "outcome": "running",
		}
		appendValidationAttempt(task, attempt)
		if err := saveValidationState(store, task); err != nil {
			return false, err
		}
		result := runValidationProcess(
			command, directories[index], taskEnvironment(candidateTask),
			timeout,
			func(owner Record) error {
				if owner == nil {
					return fail("cannot record validation process identity")
				}
				attempt["process"] = cloneRecord(owner)
				task["validation_process"] = owner
				return saveValidationState(store, task)
			},
		)
		recordValidationOutput(attempt, result)
		delete(task, "validation_process")
		if err := saveValidationState(store, task); err != nil {
			return false, err
		}
		waitErr, timedOut := result.WaitErr, result.TimedOut
		if waitErr != nil && exitCode(waitErr) == 127 && !timedOut {
			return validationFailed(store, task, Record{"command": stringsToAny(command), "reason": waitErr.Error()})
		}
		if unchanged, reason := candidateUnchanged(candidate, expectedHead); !unchanged {
			return validationFailed(store, task, Record{"command": stringsToAny(command), "exit_code": exitCode(waitErr), "reason": reason})
		}
		if timedOut {
			return validationFailed(store, task, Record{"command": stringsToAny(command), "exit_code": 124, "reason": fmt.Sprintf("validation exceeded %g seconds", timeoutSeconds)})
		}
		if waitErr != nil {
			return validationFailed(store, task, Record{"command": stringsToAny(command), "exit_code": exitCode(waitErr)})
		}
	}
	delete(task, "validation_failure")
	if err := saveValidationState(store, task); err != nil {
		return false, err
	}
	return true, nil
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
		conflicts, conflictErr := gitCommand(path, true, "diff", "--name-only", "--diff-filter=U")
		if conflictErr != nil {
			return "", false, conflictErr
		}
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
	fastForward, err := isAncestorChecked(repository, targetSHA, resultCommit)
	if err != nil {
		return "", "", err
	}
	if fastForward {
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

func deferIntegration(store *Store, task Record, status, reason string, repositoryReserved bool) (bool, error) {
	if err := setStatus(store, task, status, reason); err != nil {
		return false, err
	}
	_, err := cleanupTask(store, task, repositoryReserved, false)
	return false, err
}

func queuedReason(store *Store, repository string, task Record, reason string) string {
	if count := notifyActiveSessions(store, repository, task); count > 0 {
		return fmt.Sprintf("%s; handoff requested from %d active session(s)", reason, count)
	}
	return reason
}

func integrateTask(store *Store, task Record) (bool, error) {
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
	defer func() { _ = integrationLock.Unlock() }()
	targetSHA, targetExists, err := branchRef(repository, target)
	if err != nil {
		return deferIntegration(store, task, StatusRecovery, "cannot inspect target branch: "+err.Error(), false)
	}
	if targetExists {
		alreadyPresent, ancestryErr := isAncestorChecked(repository, resultCommit, targetSHA)
		if ancestryErr != nil {
			return deferIntegration(store, task, StatusRecovery, ancestryErr.Error(), false)
		}
		if alreadyPresent {
			task["integrated_commit"] = targetSHA
			task["integration_strategy"] = "already-present"
			delete(task, "integration_redundant_result")
			if err := setStatus(store, task, StatusIntegrated, ""); err != nil {
				return true, err
			}
			resolveTaskNotices(store, stringValue(task, "task_id"))
			if err := applyMemoryUpdate(store, task); err != nil {
				return true, err
			}
			if _, err := cleanupTask(store, task, false, false); err != nil {
				return true, err
			}
			return true, nil
		}
	}
	activity, err := store.RepositoryActivityLock(repository, true, false)
	if err != nil {
		if isLockBusy(err) {
			return deferIntegration(store, task, StatusReady, queuedReason(store, repository, task, "repository has an active agent; integration queued"), false)
		}
		return deferIntegration(store, task, StatusRecovery, err.Error(), false)
	}
	defer func() { _ = activity.Unlock() }()
	key, err := repoKey(repository)
	if err != nil {
		return deferIntegration(store, task, StatusRecovery, err.Error(), true)
	}
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
			if isLockBusy(lockErr) {
				return deferIntegration(store, task, StatusReady, queuedReason(store, repository, task, "checkout has an active agent; integration queued: "+path), true)
			}
			return deferIntegration(store, task, StatusRecovery, lockErr.Error(), true)
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

func integrateTaskReserved(store *Store, task Record, repository, target, resultCommit, candidate string) (success bool, resultErr error) {
	targetSHA, err := gitRef(repository, "refs/heads/"+target)
	if err != nil {
		return deferIntegration(store, task, StatusRecovery, "cannot resolve target branch "+target+": "+err.Error(), true)
	}
	base := stringValue(task, "base_sha")
	if base == "" {
		return deferIntegration(store, task, StatusRecovery, "result does not descend from the recorded base", true)
	}
	resultDescends, ancestryErr := isAncestorChecked(repository, base, resultCommit)
	if ancestryErr != nil {
		return deferIntegration(store, task, StatusRecovery, ancestryErr.Error(), true)
	}
	if !resultDescends {
		return deferIntegration(store, task, StatusRecovery, "result does not descend from the recorded base", true)
	}
	findings, err := forbiddenHistory(repository, resultCommit, targetSHA)
	if err != nil {
		return deferIntegration(store, task, StatusRecovery, err.Error(), true)
	}
	if len(findings) > 0 {
		task["forbidden_history"] = recordsToAny(findings)
		return deferIntegration(store, task, StatusRecovery, "result history tracks forbidden paths: "+strings.Join(findingPaths(findings), ", "), true)
	}
	alreadyPresent, ancestryErr := isAncestorChecked(repository, resultCommit, targetSHA)
	if ancestryErr != nil {
		return deferIntegration(store, task, StatusRecovery, ancestryErr.Error(), true)
	}
	if alreadyPresent {
		task["integrated_commit"] = targetSHA
		if err := setStatus(store, task, StatusIntegrated, "result was already present on target"); err != nil {
			return true, err
		}
		resolveTaskNotices(store, stringValue(task, "task_id"))
		if err := applyMemoryUpdate(store, task); err != nil {
			return true, err
		}
		if _, err := cleanupTask(store, task, true, false); err != nil {
			return true, err
		}
		return true, nil
	}
	targetDescends, ancestryErr := isAncestorChecked(repository, base, targetSHA)
	if ancestryErr != nil {
		return deferIntegration(store, task, StatusRecovery, ancestryErr.Error(), true)
	}
	if !targetDescends {
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
	if err := setStatus(store, task, StatusIntegrating, ""); err != nil {
		return false, err
	}
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
		if err := store.Save(task); err != nil && resultErr == nil {
			resultErr = err
		}
	}()
	candidateHead, strategy, err := createIntegrationCandidate(repository, task, targetSHA, resultCommit, candidate)
	if err != nil {
		return deferIntegration(store, task, StatusRecovery, err.Error(), true)
	}
	if candidateHead == "" {
		return deferIntegration(store, task, StatusRecovery, "integration conflict; committed result preserved", true)
	}
	if err := setStatus(store, task, StatusValidating, ""); err != nil {
		return false, err
	}
	valid, validationErr := validateCandidate(store, task, candidate, targetSHA, candidateHead)
	if validationErr != nil {
		return deferIntegration(store, task, StatusRecovery, "integration candidate state could not be recorded: "+validationErr.Error(), true)
	}
	if !valid {
		return deferIntegration(store, task, StatusRecovery, "integration candidate failed validation", true)
	}
	if unchanged, reason := candidateUnchanged(candidate, candidateHead); !unchanged {
		task["validation_failure"] = Record{"reason": reason}
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
		if err := setStatus(store, task, StatusIntegrated, "result changes were already present on target"); err != nil {
			return false, err
		}
		resolveTaskNotices(store, stringValue(task, "task_id"))
		if err := applyMemoryUpdate(store, task); err != nil {
			return true, err
		}
	} else {
		if reason := advanceIntegrationTarget(repository, target, targetSHA, candidateHead, checkout); reason != "" {
			return deferIntegration(store, task, StatusReady, reason+"; integration queued", true)
		}
		task["integrated_commit"] = candidateHead
		task["integration_strategy"] = strategy
		delete(task, "integration_redundant_result")
		if err := setStatus(store, task, StatusIntegrated, ""); err != nil {
			return true, err
		}
		resolveTaskNotices(store, stringValue(task, "task_id"))
		if err := applyMemoryUpdate(store, task); err != nil {
			return true, err
		}
	}
	if _, err := cleanupTask(store, task, true, false); err != nil {
		return true, err
	}
	return stringValue(task, "integrated_commit") != "", nil
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
		_, err := integrateTask(store, task)
		return err
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
	branch, err := gitCommand(path, true, "branch", "--show-current")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(branch.Stdout) != stringValue(task, "branch") {
		return nil, fail("unexpected task branch: %s", strings.TrimSpace(branch.Stdout))
	}
	resultCommit, err := gitRef(path, "HEAD")
	if err != nil {
		return nil, err
	}
	base := stringValue(task, "base_sha")
	repository := stringValue(task, "repository")
	if base == "" {
		return nil, fail("publish result does not descend from the recorded task base")
	}
	resultDescends, err := isAncestorChecked(repository, base, resultCommit)
	if err != nil {
		return nil, err
	}
	if !resultDescends {
		return nil, fail("publish result does not descend from the recorded task base")
	}
	target := stringValue(task, "target_branch")
	if target == "" {
		return nil, fail("publish target branch is unavailable: %s", firstNonempty(target, "(missing)"))
	}
	_, targetExists, err := branchRef(repository, target)
	if err != nil {
		return nil, err
	}
	if !targetExists {
		return nil, fail("publish target branch is unavailable: %s", target)
	}
	key, err := repoKey(repository)
	if err != nil {
		return nil, err
	}
	candidate := filepath.Join(store.Integrations, key, stringValue(task, "task_id")+"-publish")
	defer removeIntegrationWorktree(repository, candidate)
	publishLock, err := store.Lock("publish:"+stringValue(task, "task_id"), false)
	if err != nil {
		return nil, fail("another publish or integration is running")
	}
	defer func() { _ = publishLock.Unlock() }()
	integrationLock, err := store.Lock("integrate:"+stringValue(task, "git_common_dir")+":"+target, false)
	if err != nil {
		return nil, fail("another publish or integration is running")
	}
	defer func() { _ = integrationLock.Unlock() }()
	activity, err := store.RepositoryActivityLock(repository, false, false)
	if err != nil {
		return nil, fail("repository has another active lifecycle operation")
	}
	defer func() { _ = activity.Unlock() }()
	targetSHA, err := gitRef(repository, "refs/heads/"+target)
	if err != nil {
		return nil, err
	}
	targetDescends, err := isAncestorChecked(repository, base, targetSHA)
	if err != nil {
		return nil, err
	}
	if !targetDescends {
		return nil, fail("target no longer descends from the recorded task base")
	}
	alreadyPresent, err := isAncestorChecked(repository, resultCommit, targetSHA)
	if err != nil {
		return nil, err
	}
	if alreadyPresent {
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
		defer func() { _ = checkoutLock.Unlock() }()
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
	if err != nil {
		return nil, err
	}
	if candidateHead == "" {
		return nil, fail("publish candidate conflicts with the current target")
	}
	validationTask := cloneRecord(task)
	valid, validationErr := validateCandidate(nil, validationTask, candidate, targetSHA, candidateHead)
	if validationErr != nil {
		return nil, validationErr
	}
	if !valid {
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
