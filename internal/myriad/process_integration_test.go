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

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid pid in %s: %q, %v", path, raw, err)
	}
	return pid
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

func TestLaterSessionRebasesAfterConcurrentPublish(t *testing.T) {
	myriad, helper := testMyriadBinaries(t)
	repository := testRepository(t)
	store := testStore(t)
	root := t.TempDir()
	yReady := filepath.Join(root, "y-ready")
	yRelease := filepath.Join(root, "y-release")
	y := exec.Command(myriad, "start", "--agent", "custom", "--task", "session-y", "--quiet", "--", helper, "commit-wait", "y-result.txt", yReady, yRelease)
	y.Dir = repository
	y.Env = os.Environ()
	if err := y.Start(); err != nil {
		t.Fatal(err)
	}
	yWaited := false
	defer func() {
		_ = os.WriteFile(yRelease, []byte("release\n"), 0o600)
		if !yWaited {
			_ = y.Process.Kill()
			_ = y.Wait()
		}
		for _, task := range store.All(false) {
			owner := recordMap(task, "process")
			if processAlive(owner) {
				pid, _ := intValue(owner["pid"])
				_ = unix.Kill(pid, unix.SIGKILL)
			}
		}
	}()
	waitForFile(t, yReady)

	x := exec.Command(myriad, "start", "--agent", "custom", "--task", "session-x", "--quiet", "--", helper, "commit-publish", "x-result.txt", myriad)
	x.Dir = repository
	x.Env = os.Environ()
	if output, err := x.CombinedOutput(); err != nil {
		t.Fatalf("session X could not publish while Y was active: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(repository, "x-result.txt")); err != nil {
		t.Fatalf("session X result was not published: %v", err)
	}
	if processStart(readPIDFile(t, yReady)) == "" {
		t.Fatal("publishing session X stopped active session Y")
	}

	if err := os.WriteFile(yRelease, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := y.Wait(); err != nil {
		t.Fatalf("session Y did not integrate after X advanced main: %v", err)
	}
	yWaited = true
	for _, name := range []string{"x-result.txt", "y-result.txt"} {
		if _, err := os.Stat(filepath.Join(repository, name)); err != nil {
			t.Fatalf("final main is missing %s: %v", name, err)
		}
	}
	var xTask, yTask Record
	for _, task := range store.All(true) {
		switch stringValue(task, "description") {
		case "session-x":
			xTask = task
		case "session-y":
			yTask = task
		}
	}
	if xTask == nil || stringValue(xTask, "status") != StatusIntegrated {
		t.Fatalf("session X did not finish integrated: %s", describe(xTask))
	}
	if yTask == nil || stringValue(yTask, "status") != StatusIntegrated || stringValue(yTask, "integration_strategy") != "rebase" {
		t.Fatalf("session Y was not automatically rebased: %s", describe(yTask))
	}
}

func TestLauncherPreservesLivePublishMetadata(t *testing.T) {
	myriad, helper := testMyriadBinaries(t)
	repository := testRepository(t)
	store := testStore(t)
	root := t.TempDir()
	ready := filepath.Join(root, "publish-ready")
	trigger := filepath.Join(root, "publish-trigger")
	command := exec.Command(myriad, "start", "--agent", "custom", "--task", "preserve-live-publish", "--quiet", "--", helper, "commit-publish-wait", "task-result.txt", ready, trigger, myriad)
	command.Dir = repository
	command.Env = os.Environ()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		_ = os.WriteFile(trigger, []byte("publish\n"), 0o600)
		if !waited {
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
	target := testCommitFile(t, repository, "target-result.txt", "target\n", "feat: advance target before publish")
	if err := os.WriteFile(trigger, []byte("publish\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("managed publish session failed: %v", err)
	}
	waited = true
	tasks := store.All(true)
	if len(tasks) != 1 {
		t.Fatalf("task count = %d, want 1: %s", len(tasks), describe(tasks))
	}
	task := tasks[0]
	if stringValue(task, "status") != StatusIntegrated || stringValue(task, "base_sha") != target {
		t.Fatalf("launcher overwrote live publish metadata: %s", describe(task))
	}
	if stringValue(task, "published_rebased_from_base") == "" {
		t.Fatal("published rebase origin was not retained")
	}
}

func TestProvisioningDoesNotFallBackAfterAppServerFailure(t *testing.T) {
	myriad, _ := testMyriadBinaries(t)
	repository := testRepository(t)
	store := testStore(t)
	tools := t.TempDir()
	marker := filepath.Join(t.TempDir(), "direct-codex-launched")
	fakeCodex := filepath.Join(tools, "codex")
	script := `#!/bin/sh
for argument do
    if [ "$argument" = app-server ]; then
        exit 42
    fi
done
printf 'launched\n' >"$MYRIAD_TEST_CODEX_LAUNCHED"
`
	if err := os.WriteFile(fakeCodex, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(myriad, "start", "--agent", "codex", "--task", "incompatible-codex", "--quiet", "--", "codex")
	command.Dir = repository
	command.Env = overlayEnvironment(os.Environ(), map[string]string{
		"PATH":                       tools + string(os.PathListSeparator) + os.Getenv("PATH"),
		"MYRIAD_TEST_CODEX_LAUNCHED": marker,
	})
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("managed launch ignored App Server failure: %s", output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Codex was launched without its provisioning hook: %v", err)
	}
	tasks := store.All(true)
	if len(tasks) != 1 {
		t.Fatalf("task count = %d, want 1: %s", len(tasks), describe(tasks))
	}
	if status := stringValue(tasks[0], "status"); status != StatusRecovery {
		t.Fatalf("failed provisioning status = %s, reason = %s", status, stringValue(tasks[0], "status_reason"))
	}
	refreshInterruptedTasks(store, repository)
	current, err := store.Load(stringValue(tasks[0], "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	if status := stringValue(current, "status"); status != StatusCompleted {
		t.Fatalf("empty failed provisioning was not resolved: %s", status)
	}
	if _, err := os.Stat(stringValue(current, "worktree_path")); !os.IsNotExist(err) {
		t.Fatalf("resolved provisioning failure left a reserved worktree: %v", err)
	}
}

func TestRepeatedInterruptOrEOFDuringHookCleansUp(t *testing.T) {
	myriad, helper := testMyriadBinaries(t)
	for _, termination := range []string{"interrupt", "eof"} {
		t.Run(termination, func(t *testing.T) {
			repository := testRepository(t)
			store := testStore(t)
			root := t.TempDir()
			agentReady := filepath.Join(root, "agent-ready")
			hookPIDPath := filepath.Join(root, "hook.pid")
			stdin, input, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command(myriad, "start", "--agent", "custom", "--task", "interrupt-hook-"+termination, "--quiet", "--", helper, "interrupt-hook", agentReady, hookPIDPath)
			command.Dir = repository
			command.Env = os.Environ()
			command.Stdin = stdin
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := command.Start(); err != nil {
				_ = stdin.Close()
				_ = input.Close()
				t.Fatal(err)
			}
			_ = stdin.Close()
			waited := false
			defer func() {
				_ = input.Close()
				if !waited {
					_ = unix.Kill(-command.Process.Pid, unix.SIGKILL)
					_ = command.Wait()
				}
			}()

			waitForFile(t, agentReady)
			if err := unix.Kill(-command.Process.Pid, unix.SIGINT); err != nil {
				t.Fatal(err)
			}
			waitForFile(t, hookPIDPath)
			raw, err := os.ReadFile(hookPIDPath)
			if err != nil {
				t.Fatal(err)
			}
			hookPID, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			if termination == "interrupt" {
				if err := unix.Kill(-command.Process.Pid, unix.SIGINT); err != nil {
					t.Fatal(err)
				}
			} else if err := input.Close(); err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case err := <-done:
				waited = true
				if err != nil {
					t.Fatalf("managed session did not exit cleanly: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("managed session did not finish after repeated termination input")
			}
			waitForProcessExit(t, hookPID)
			tasks := store.All(true)
			if len(tasks) != 1 || stringValue(tasks[0], "status") != StatusCompleted {
				t.Fatalf("unexpected task result: %s", describe(tasks))
			}
			if _, err := os.Stat(stringValue(tasks[0], "worktree_path")); !os.IsNotExist(err) {
				t.Fatalf("completed interrupted worktree still exists: %v", err)
			}
		})
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

func TestForcedSessionTerminationIsIsolatedAndRecoverable(t *testing.T) {
	myriad, helper := testMyriadBinaries(t)
	for _, termination := range []string{"agent", "supervisor", "launcher"} {
		t.Run(termination, func(t *testing.T) {
			repository := testRepository(t)
			store := testStore(t)
			root := t.TempDir()
			targetReady := filepath.Join(root, "target-ready")
			targetRelease := filepath.Join(root, "target-release")
			peerReady := filepath.Join(root, "peer-ready")
			peerRelease := filepath.Join(root, "peer-release")

			target := exec.Command(myriad, "start", "--agent", "custom", "--task", "forced-target", "--quiet", "--", helper, "commit-wait", "forced-result.txt", targetReady, targetRelease)
			target.Dir = repository
			target.Env = os.Environ()
			peer := exec.Command(myriad, "start", "--agent", "custom", "--task", "unaffected-peer", "--quiet", "--", helper, "wait", peerReady, peerRelease)
			peer.Dir = repository
			peer.Env = os.Environ()
			if err := target.Start(); err != nil {
				t.Fatal(err)
			}
			targetWaited := false
			peerStarted := false
			peerWaited := false
			defer func() {
				_ = os.WriteFile(targetRelease, []byte("release\n"), 0o600)
				_ = os.WriteFile(peerRelease, []byte("release\n"), 0o600)
				if !targetWaited {
					_ = target.Process.Kill()
					_ = target.Wait()
				}
				if peerStarted && !peerWaited {
					_ = peer.Process.Kill()
					_ = peer.Wait()
				}
				for _, task := range store.All(false) {
					owner := recordMap(task, "process")
					if processAlive(owner) {
						pid, _ := intValue(owner["pid"])
						_ = unix.Kill(pid, unix.SIGKILL)
					}
				}
			}()
			if err := peer.Start(); err != nil {
				t.Fatal(err)
			}
			peerStarted = true
			waitForFile(t, targetReady)
			waitForFile(t, peerReady)

			targetAgentPID := readPIDFile(t, targetReady)
			peerAgentPID := readPIDFile(t, peerReady)
			var targetTask, peerTask Record
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				for _, task := range store.All(false) {
					switch stringValue(task, "description") {
					case "forced-target":
						targetTask = task
					case "unaffected-peer":
						peerTask = task
					}
				}
				if targetTask != nil && peerTask != nil && processAlive(targetTask["process"]) && processAlive(peerTask["process"]) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if targetTask == nil || peerTask == nil {
				t.Fatalf("concurrent running tasks did not appear: %s", describe(store.All(true)))
			}
			targetSupervisorPID, _ := intValue(recordMap(targetTask, "process")["pid"])
			if targetSupervisorPID <= 1 {
				t.Fatal("target task has no valid supervisor pid")
			}

			switch termination {
			case "agent":
				if err := unix.Kill(targetAgentPID, unix.SIGKILL); err != nil {
					t.Fatal(err)
				}
			case "supervisor":
				if err := unix.Kill(targetSupervisorPID, unix.SIGKILL); err != nil {
					t.Fatal(err)
				}
			case "launcher":
				if err := target.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if err := target.Wait(); err == nil {
					t.Fatal("SIGKILLed target launcher unexpectedly exited successfully")
				}
				targetWaited = true
				if processStart(targetAgentPID) == "" || !processAlive(targetTask["process"]) {
					t.Fatal("launcher death prematurely stopped the supervised target session")
				}
				if err := unix.Kill(targetSupervisorPID, unix.SIGKILL); err != nil {
					t.Fatal(err)
				}
			}
			waitForProcessExit(t, targetAgentPID)
			if termination != "launcher" {
				if err := target.Wait(); err == nil {
					t.Fatal("force-terminated target unexpectedly exited successfully")
				}
				targetWaited = true
			} else {
				waitForProcessExit(t, targetSupervisorPID)
				refreshInterruptedTasks(store, repository)
			}

			if processStart(peerAgentPID) == "" {
				t.Fatal("terminating one session stopped its peer agent")
			}
			currentPeer, err := store.Load(stringValue(peerTask, "task_id"))
			if err != nil {
				t.Fatal(err)
			}
			if !processAlive(currentPeer["process"]) {
				t.Fatal("terminating one session stopped its peer supervisor")
			}
			identity, err := taskCheckoutIdentity(currentPeer)
			if err != nil {
				t.Fatal(err)
			}
			if lock, err := store.CheckoutLock(stringValue(currentPeer, "worktree_path"), identity, false); err == nil {
				_ = lock.Unlock()
				t.Fatal("peer checkout lease was released by another session's termination")
			} else if !isLockBusy(err) {
				t.Fatal(err)
			}

			if err := os.WriteFile(peerRelease, []byte("release\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := peer.Wait(); err != nil {
				t.Fatalf("unaffected peer did not finish cleanly: %v", err)
			}
			peerWaited = true

			currentTarget, err := store.Load(stringValue(targetTask, "task_id"))
			if err != nil {
				t.Fatal(err)
			}
			if status := stringValue(currentTarget, "status"); status != StatusRecovery {
				t.Fatalf("force-terminated task status = %s, reason = %s", status, stringValue(currentTarget, "status_reason"))
			}
			result := stringValue(currentTarget, "result_commit")
			if result == "" {
				t.Fatal("force-terminated task did not preserve its committed result")
			}

			recovered := exec.Command(myriad, "recover", stringValue(currentTarget, "task_id"), "--agent", "custom", "--new-session", "--quiet", "--", helper, "noop", "unused")
			recovered.Dir = repository
			recovered.Env = os.Environ()
			if output, err := recovered.CombinedOutput(); err != nil {
				t.Fatalf("force-terminated task recovery failed: %v\n%s", err, output)
			}
			integrated, err := store.Load(stringValue(currentTarget, "task_id"))
			if err != nil {
				t.Fatal(err)
			}
			if status := stringValue(integrated, "status"); status != StatusIntegrated {
				t.Fatalf("recovered task status = %s, reason = %s", status, stringValue(integrated, "status_reason"))
			}
			if got := stringValue(integrated, "integrated_commit"); got != result {
				t.Fatalf("integrated commit = %s, preserved result = %s", got, result)
			}
			contents, err := os.ReadFile(filepath.Join(repository, "forced-result.txt"))
			if err != nil || string(contents) != "committed by agent\n" {
				t.Fatalf("recovered result was not integrated: %q, %v", contents, err)
			}
		})
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

func TestValidationDoesNotReadInteractiveTerminal(t *testing.T) {
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("util-linux script is unavailable")
	}
	myriad, helper := testMyriadBinaries(t)
	repository := testRepository(t)
	store := testStore(t)
	check := displayCommand([]string{helper, "stdin-eof", "unused"})
	arguments := []string{
		myriad, "start", "--agent", "custom", "--task", "noninteractive-validation",
		"--check", check, "--check-timeout", "0.5", "--quiet", "--",
		helper, "commit", "validated-result.txt",
	}
	command := exec.Command("script", "--quiet", "--return", "--command", displayCommand(arguments), "/dev/null")
	command.Dir = repository
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("managed validation inherited interactive stdin: %v\n%s", err, output)
	}
	tasks := store.All(true)
	if len(tasks) != 1 || stringValue(tasks[0], "status") != StatusIntegrated {
		t.Fatalf("unexpected validation result: %s", describe(tasks))
	}
	attempts := recordSlice(tasks[0], "validation_attempts")
	if len(attempts) != 1 {
		t.Fatalf("validation attempt count = %d, want 1", len(attempts))
	}
	attempt := anyRecord(attempts[0])
	if stringValue(attempt, "outcome") != "passed" || !strings.Contains(stringValue(attempt, "stdout_tail"), "noninteractive") {
		t.Fatalf("validation diagnostics were not retained: %s", describe(attempt))
	}
}

func TestStoppedValidationFailsPromptly(t *testing.T) {
	_, helper := testMyriadBinaries(t)
	repository := testRepository(t)
	store := testStore(t)
	check := displayCommand([]string{helper, "stop-group", "unused"})
	task := testTask(t, store, repository, createTaskOptions{Checks: []string{check}, CheckTimeout: 60})
	testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "task\n", "feat: add stopped validation result")

	started := time.Now()
	current := finishTestTask(t, store, task, true)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("stopped validation took %s to fail", elapsed)
	}
	if status := stringValue(current, "status"); status != StatusRecovery {
		t.Fatalf("status = %s, want %s", status, StatusRecovery)
	}
	failure := recordMap(current, "validation_failure")
	if reason := stringValue(failure, "reason"); !strings.Contains(reason, "stopped by job control") {
		t.Fatalf("unexpected validation failure: %s", describe(failure))
	}
	attempts := recordSlice(current, "validation_attempts")
	if len(attempts) != 1 || stringValue(anyRecord(attempts[0]), "outcome") != "stopped" {
		t.Fatalf("stopped attempt was not retained: %s", describe(attempts))
	}
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
