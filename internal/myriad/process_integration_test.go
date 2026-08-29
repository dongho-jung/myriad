package myriad

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	defer output.Close()
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
