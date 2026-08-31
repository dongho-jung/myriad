package myriad

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func testMyriadBinaries(t *testing.T) (string, string) {
	t.Helper()
	project, err := repoRoot(currentDirectory())
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	myriad := filepath.Join(directory, "myriad")
	helper := filepath.Join(directory, "agenthelper")
	testCommand(t, project, "go", "build", "-o", myriad, "./cmd/myriad")
	testCommand(t, project, "go", "build", "-o", helper, "./internal/myriad/testdata/agenthelper")
	return myriad, helper
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if processStart(pid) == "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d did not exit", pid)
}

func TestProcessAliveRejectsZombie(t *testing.T) {
	command := exec.Command("sleep", "0.05")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	record := processRecord(command.Process.Pid, "test-child", 0)
	if record == nil {
		_ = command.Wait()
		t.Fatal("could not record child process identity")
	}
	defer func() { _ = command.Wait() }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, state := processIdentity(command.Process.Pid)
		if state == 'Z' {
			if processAlive(record) {
				t.Fatal("zombie child was reported as alive")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child did not enter zombie state before being reaped")
}

func TestSupervisorStopsDetachedDescendantBeforeReturning(t *testing.T) {
	myriad, helper := testMyriadBinaries(t)
	repository := testRepository(t)
	store := testStore(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	command := exec.Command(myriad, "start", "--agent", "custom", "--task", "supervise-descendant", "--quiet", "--", helper, "detach", pidFile)
	command.Dir = repository
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("myriad start failed: %v\n%s", err, output)
	}
	waitForFile(t, pidFile)
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	waitForProcessExit(t, pid)
	tasks := store.All(true)
	if len(tasks) != 1 || stringValue(tasks[0], "status") != StatusCompleted {
		t.Fatalf("unexpected task result: %s", describe(tasks))
	}
}

func TestManagedStartIntegratesCommittedResult(t *testing.T) {
	myriad, helper := testMyriadBinaries(t)
	repository := testRepository(t)
	store := testStore(t)
	command := exec.Command(myriad, "start", "--agent", "custom", "--task", "complete-primary-lifecycle", "--quiet", "--", helper, "commit", "agent-result.txt")
	command.Dir = repository
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("managed start failed: %v\n%s", err, output)
	}

	contents, err := os.ReadFile(filepath.Join(repository, "agent-result.txt"))
	if err != nil || string(contents) != "committed by agent\n" {
		t.Fatalf("managed result was not integrated: %q, %v", contents, err)
	}
	tasks := store.All(true)
	if len(tasks) != 1 {
		t.Fatalf("task count = %d, want 1: %s", len(tasks), describe(tasks))
	}
	task := tasks[0]
	if status := stringValue(task, "status"); status != StatusIntegrated {
		t.Fatalf("task status = %s, reason = %s", status, stringValue(task, "status_reason"))
	}
	if integrated := stringValue(task, "integrated_commit"); integrated == "" {
		t.Fatal("task did not record its integrated commit")
	} else if head, _ := gitRef(repository, "refs/heads/main"); head != integrated {
		t.Fatalf("main = %s, integrated commit = %s", head, integrated)
	}
	if _, err := os.Stat(stringValue(task, "worktree_path")); !os.IsNotExist(err) {
		t.Fatalf("managed worktree still exists: %v", err)
	}
	if branchExists(repository, stringValue(task, "branch")) {
		t.Fatal("integrated task branch still exists")
	}
}

func TestManagedRecoveryIntegratesPreservedCommit(t *testing.T) {
	myriad, helper := testMyriadBinaries(t)
	repository := testRepository(t)
	store := testStore(t)
	failed := exec.Command(myriad, "start", "--agent", "custom", "--task", "recover-committed-result", "--quiet", "--", helper, "commit-fail", "recovered-result.txt")
	failed.Dir = repository
	failed.Env = os.Environ()
	if output, err := failed.CombinedOutput(); err == nil {
		t.Fatalf("failing managed agent exited successfully: %s", output)
	}

	tasks := store.All(true)
	if len(tasks) != 1 {
		t.Fatalf("task count = %d, want 1: %s", len(tasks), describe(tasks))
	}
	task := tasks[0]
	if status := stringValue(task, "status"); status != StatusRecovery {
		t.Fatalf("failed task status = %s, reason = %s", status, stringValue(task, "status_reason"))
	}
	result := stringValue(task, "result_commit")
	if result == "" {
		t.Fatal("failed agent's committed result was not preserved")
	}
	if !branchExists(repository, stringValue(task, "branch")) {
		t.Fatal("failed agent's committed branch was not preserved")
	}

	recovered := exec.Command(myriad, "recover", stringValue(task, "task_id"), "--agent", "custom", "--new-session", "--quiet", "--", helper, "noop", "unused")
	recovered.Dir = repository
	recovered.Env = os.Environ()
	if output, err := recovered.CombinedOutput(); err != nil {
		t.Fatalf("managed recovery failed: %v\n%s", err, output)
	}
	current, err := store.Load(stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	if status := stringValue(current, "status"); status != StatusIntegrated {
		t.Fatalf("recovered task status = %s, reason = %s", status, stringValue(current, "status_reason"))
	}
	if got := stringValue(current, "integrated_commit"); got != result {
		t.Fatalf("integrated commit = %s, preserved result = %s", got, result)
	}
	contents, err := os.ReadFile(filepath.Join(repository, "recovered-result.txt"))
	if err != nil || string(contents) != "committed by agent\n" {
		t.Fatalf("recovered result was not integrated: %q, %v", contents, err)
	}
	if _, err := os.Stat(stringValue(task, "worktree_path")); !os.IsNotExist(err) {
		t.Fatalf("recovered worktree still exists: %v", err)
	}
}

func TestSupervisorKeepsAgentInForegroundProcessGroup(t *testing.T) {
	myriad, helper := testMyriadBinaries(t)
	repository := testRepository(t)
	store := testStore(t)
	root := t.TempDir()
	ready := filepath.Join(root, "ready")
	release := filepath.Join(root, "release")
	command := exec.Command(myriad, "start", "--agent", "custom", "--task", "signal-routing", "--quiet", "--", helper, "wait", ready, release)
	command.Dir = repository
	command.Env = os.Environ()
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = unix.Kill(-command.Process.Pid, unix.SIGKILL)
			_ = command.Wait()
		}
	}()
	waitForFile(t, ready)
	raw, err := os.ReadFile(ready)
	if err != nil {
		t.Fatal(err)
	}
	agentPID, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}

	var supervisorPID int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tasks := store.All(false)
		if len(tasks) == 1 && stringValue(tasks[0], "status") == StatusRunning {
			supervisorPID, _ = intValue(recordMap(tasks[0], "process")["pid"])
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if supervisorPID == 0 {
		t.Fatal("supervisor process identity did not appear")
	}
	supervisorPGID, err := unix.Getpgid(supervisorPID)
	if err != nil {
		t.Fatal(err)
	}
	if supervisorPGID != supervisorPID {
		t.Fatalf("supervisor process group = %d, want isolated group %d", supervisorPGID, supervisorPID)
	}
	agentPGID, err := unix.Getpgid(agentPID)
	if err != nil {
		t.Fatal(err)
	}
	if agentPGID != command.Process.Pid {
		t.Fatalf("agent process group = %d, want launcher foreground group %d", agentPGID, command.Process.Pid)
	}

	if err := unix.Kill(-command.Process.Pid, unix.SIGINT); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-done:
		waited = true
	case <-time.After(10 * time.Second):
		t.Fatal("foreground signal did not stop the supervised agent")
	}
}

func TestSupervisorDeathStopsAgentBeforeLeaseRelease(t *testing.T) {
	myriad, helper := testMyriadBinaries(t)
	repository := testRepository(t)
	store := testStore(t)
	root := t.TempDir()
	ready := filepath.Join(root, "ready")
	release := filepath.Join(root, "release")
	command := exec.Command(myriad, "start", "--agent", "custom", "--task", "supervisor-death", "--quiet", "--", helper, "wait", ready, release)
	command.Dir = repository
	command.Env = os.Environ()
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
		if !waited {
			_ = unix.Kill(-command.Process.Pid, unix.SIGKILL)
			_ = command.Wait()
		}
	}()
	waitForFile(t, ready)
	raw, err := os.ReadFile(ready)
	if err != nil {
		t.Fatal(err)
	}
	agentPID, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}

	var task Record
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tasks := store.All(false)
		if len(tasks) == 1 && stringValue(tasks[0], "status") == StatusRunning {
			task = tasks[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if task == nil {
		t.Fatal("supervisor process identity did not appear")
	}
	supervisorPID, _ := intValue(recordMap(task, "process")["pid"])
	if supervisorPID <= 1 {
		t.Fatal("task has no valid supervisor pid")
	}
	if err := unix.Kill(supervisorPID, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitForProcessExit(t, agentPID)

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-done:
		waited = true
	case <-time.After(10 * time.Second):
		t.Fatal("launcher did not finish after supervisor death")
	}
	identity, err := taskCheckoutIdentity(task)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := store.CheckoutLock(stringValue(task, "worktree_path"), identity, false)
	if err != nil {
		t.Fatalf("checkout lease remained after agent termination: %v", err)
	}
	_ = lock.Unlock()
}

func TestSupervisorKeepsCheckoutLockedAfterLauncherDies(t *testing.T) {
	myriad, helper := testMyriadBinaries(t)
	repository := testRepository(t)
	store := testStore(t)
	root := t.TempDir()
	ready := filepath.Join(root, "ready")
	release := filepath.Join(root, "release")
	outputPath := filepath.Join(root, "output.log")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close() }()
	command := exec.Command(myriad, "start", "--agent", "custom", "--task", "hold-checkout", "--quiet", "--", helper, "wait", ready, release)
	command.Dir = repository
	command.Env = os.Environ()
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	launcherWaited := false
	defer func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
		if !launcherWaited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		for _, task := range store.All(false) {
			owner := recordMap(task, "process")
			if processAlive(owner) {
				pid, _ := intValue(owner["pid"])
				_ = unix.Kill(pid, unix.SIGKILL)
			}
		}
	}()
	waitForFile(t, ready)

	var task Record
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tasks := store.All(false)
		if len(tasks) == 1 && stringValue(tasks[0], "status") == StatusRunning {
			task = tasks[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if task == nil {
		_ = output.Sync()
		contents, _ := os.ReadFile(outputPath)
		t.Fatalf("running task did not appear: %s", contents)
	}
	identity, err := taskCheckoutIdentity(task)
	if err != nil {
		t.Fatal(err)
	}
	if lock, err := store.CheckoutLock(stringValue(task, "worktree_path"), identity, false); err == nil {
		_ = lock.Unlock()
		t.Fatal("checkout lock was free while the agent was running")
	} else if !isLockBusy(err) {
		t.Fatal(err)
	}

	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("SIGKILLed launcher unexpectedly exited successfully")
	}
	launcherWaited = true
	if lock, err := store.CheckoutLock(stringValue(task, "worktree_path"), identity, false); err == nil {
		_ = lock.Unlock()
		t.Fatal("supervisor released the checkout when only the launcher died")
	} else if !isLockBusy(err) {
		t.Fatal(err)
	}

	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		lock, err := store.CheckoutLock(stringValue(task, "worktree_path"), identity, false)
		if err == nil {
			_ = lock.Unlock()
			return
		}
		if !isLockBusy(err) && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("checkout lock was not released after the supervised agent exited")
}

func TestValidationStopsDetachedDescendant(t *testing.T) {
	_, helper := testMyriadBinaries(t)
	repository := testRepository(t)
	store := testStore(t)
	pidFile := filepath.Join(t.TempDir(), "validation-child.pid")
	check := displayCommand([]string{helper, "detach", pidFile})
	task := testTask(t, store, repository, createTaskOptions{Checks: []string{check}})
	testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "task\n", "feat: add validated result")

	current := finishTestTask(t, store, task, true)
	if status := stringValue(current, "status"); status != StatusIntegrated {
		t.Fatalf("status = %s, reason = %s", status, stringValue(current, "status_reason"))
	}
	waitForFile(t, pidFile)
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	waitForProcessExit(t, pid)
}

func TestValidationTimeoutPreservesResult(t *testing.T) {
	_, helper := testMyriadBinaries(t)
	repository := testRepository(t)
	store := testStore(t)
	root := t.TempDir()
	ready := filepath.Join(root, "validation-ready")
	release := filepath.Join(root, "never-released")
	check := displayCommand([]string{helper, "wait", ready, release})
	task := testTask(t, store, repository, createTaskOptions{
		Checks: []string{check}, CheckTimeout: 0.1,
	})
	testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "task\n", "feat: add timed result")
	targetBefore, _ := gitRef(repository, "refs/heads/main")

	current := finishTestTask(t, store, task, true)
	if status := stringValue(current, "status"); status != StatusRecovery {
		t.Fatalf("status = %s, want %s", status, StatusRecovery)
	}
	failure := recordMap(current, "validation_failure")
	if reason := stringValue(failure, "reason"); !strings.Contains(reason, "exceeded") {
		t.Fatalf("unexpected validation failure: %s", describe(failure))
	}
	if head, _ := gitRef(repository, "refs/heads/main"); head != targetBefore {
		t.Fatal("timed-out validation advanced the target")
	}
	if processAlive(current["validation_process"]) {
		t.Fatal("timed-out validation process was left alive")
	}
}

func TestManagedSessionAttachesAndIntegratesSecondRepository(t *testing.T) {
	myriad, helper := testMyriadBinaries(t)
	primary := testRepository(t)
	secondary := testRepository(t)
	store := testStore(t)
	resultFile := filepath.Join(t.TempDir(), "attached-worktree")
	command := exec.Command(myriad, "start", "--agent", "custom", "--task", "attach-second-repository", "--quiet", "--", helper, "attach", myriad, secondary, resultFile)
	command.Dir = primary
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("managed attachment failed: %v\n%s", err, output)
	}

	if contents, err := os.ReadFile(filepath.Join(secondary, "secondary.txt")); err != nil || string(contents) != "attached\n" {
		t.Fatalf("secondary target was not integrated: %q, %v", contents, err)
	}
	tasks := store.All(true)
	if len(tasks) != 2 {
		t.Fatalf("task count = %d, want 2: %s", len(tasks), describe(tasks))
	}
	parentStatus, attachmentStatus := "", ""
	for _, task := range tasks {
		if stringValue(task, "attachment_parent_task_id") == "" {
			parentStatus = stringValue(task, "status")
		} else {
			attachmentStatus = stringValue(task, "status")
		}
	}
	if parentStatus != StatusCompleted || attachmentStatus != StatusIntegrated {
		t.Fatalf("parent = %s, attachment = %s", parentStatus, attachmentStatus)
	}
	raw, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(strings.TrimSpace(string(raw))); !os.IsNotExist(err) {
		t.Fatalf("attached worktree was not cleaned: %v", err)
	}
}
