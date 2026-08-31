package myriad

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefreshCompletesEmptyPreProvisionRecovery(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task, err := createTask(store, createTaskOptions{
		Agent:       "codex",
		Deferred:    true,
		Description: "",
		LaunchCWD:   repository,
	})
	if err != nil {
		t.Fatal(err)
	}
	recordAgentExit(task, 1, false)
	delete(task, "process")
	if err := setStatus(store, task, StatusRecovery, "agent exited before worktree provisioning"); err != nil {
		t.Fatal(err)
	}

	refreshInterruptedTasks(store, repository)
	current, err := store.Load(stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	if status := stringValue(current, "status"); status != StatusCompleted {
		t.Fatalf("status = %s, reason = %s", status, stringValue(current, "status_reason"))
	}
	if _, err := os.Stat(stringValue(task, "worktree_path")); !os.IsNotExist(err) {
		t.Fatalf("empty reserved worktree still exists: %v", err)
	}
}

func TestRecoveryPreparationHonorsCheckoutLease(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	worktree := stringValue(task, "worktree_path")
	testCommitFile(t, worktree, "task.txt", "task\n", "fix: preserve task work")
	testCommitFile(t, repository, "target.txt", "target\n", "feat: advance target")
	task["status"] = StatusRecovery
	delete(task, "process")
	if err := store.Save(task); err != nil {
		t.Fatal(err)
	}
	identity, err := taskCheckoutIdentity(task)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.CheckoutLock(worktree, identity, false)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := prepareRecoveryCheckout(store, task); err == nil || !isLockBusy(err) {
		_ = lease.Unlock()
		t.Fatalf("recovery preparation ignored checkout lease: %v", err)
	}
	mergeHead, err := gitCommand(worktree, false, "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	if err != nil {
		_ = lease.Unlock()
		t.Fatal(err)
	}
	if mergeHead.ExitCode == 0 {
		_ = lease.Unlock()
		t.Fatal("recovery mutated the worktree while its lease was held")
	}
	if err := lease.Unlock(); err != nil {
		t.Fatal(err)
	}

	note, err := prepareRecoveryCheckout(store, task)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "staged the current target merge") {
		t.Fatalf("unexpected recovery note: %s", note)
	}
	mergeHead, err = gitCommand(worktree, false, "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	if err != nil || mergeHead.ExitCode != 0 {
		t.Fatalf("recovery did not stage the target merge: (%v, %d)", err, mergeHead.ExitCode)
	}
}

func TestTaskKeepsDotDotPrefixedWorkingDirectory(t *testing.T) {
	repository := testRepository(t)
	testCommitFile(t, repository, "..config/tracked.txt", "nested\n", "test: add nested directory")
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{
		LaunchCWD: filepath.Join(repository, "..config"),
	})
	want := filepath.Join(stringValue(task, "worktree_path"), "..config")
	if got := taskConfiguredWorkingDirectory(task); got != want {
		t.Fatalf("managed working directory = %q, want %q", got, want)
	}
	finishTestTask(t, store, task, false)
}

func TestTaskIntegratesFastForwardAndCleansUp(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	result := testCommitFile(t, stringValue(task, "worktree_path"), "tracked.txt", "changed\n", "fix: change tracked file")

	current := finishTestTask(t, store, task, true)
	if status := stringValue(current, "status"); status != StatusIntegrated {
		t.Fatalf("status = %s, reason = %s", status, stringValue(current, "status_reason"))
	}
	if integrated := stringValue(current, "integrated_commit"); integrated != result {
		t.Fatalf("integrated commit = %s, want %s", integrated, result)
	}
	if head, _ := gitRef(repository, "refs/heads/main"); head != result {
		t.Fatalf("main = %s, want %s", head, result)
	}
	if _, err := os.Stat(stringValue(task, "worktree_path")); !os.IsNotExist(err) {
		t.Fatalf("managed worktree still exists: %v", err)
	}
	if branchExists(repository, stringValue(task, "branch")) {
		t.Fatal("integrated task branch was not deleted")
	}
}

func TestCleanupRecordsBranchDeletionFailure(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	path := stringValue(task, "worktree_path")
	branch := stringValue(task, "branch")
	task["status"] = StatusCompleted
	if err := store.Save(task); err != nil {
		t.Fatal(err)
	}

	testCommand(t, path, "git", "switch", "--detach", "-q")
	holder := filepath.Join(t.TempDir(), "branch-holder")
	testCommand(t, repository, "git", "worktree", "add", "-q", holder, branch)

	cleaned, err := cleanupTask(store, task, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned {
		t.Fatal("cleanup reported success after branch deletion failed")
	}
	current, err := store.Load(stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(current, "branch_deleted_at") != "" {
		t.Fatal("failed branch deletion was recorded as successful")
	}
	if warning := stringValue(current, "cleanup_warning"); !strings.Contains(warning, "task branch deletion failed") {
		t.Fatalf("unexpected cleanup warning: %s", warning)
	}
	if !branchExists(repository, branch) {
		t.Fatal("branch disappeared despite deletion failure")
	}
}

func TestQuarantinePreservesPathWhenGitInspectionFails(t *testing.T) {
	store := testStore(t)
	path := filepath.Join(t.TempDir(), "candidate")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	task := Record{
		"task_id":    "inspection-failure",
		"repository": filepath.Join(t.TempDir(), "missing-repository"),
	}

	if _, err := quarantineUnregisteredWorktree(store, task, path); err == nil {
		t.Fatal("quarantine accepted a failed Git registration inspection")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("candidate path was moved after inspection failure: %v", err)
	}
}

func TestCleanupPreservesInaccessibleManagedPath(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	path := stringValue(task, "worktree_path")
	branch := stringValue(task, "branch")
	testCommand(t, repository, "git", "worktree", "unlock", path)
	testCommand(t, repository, "git", "worktree", "remove", path)
	if err := os.Symlink(filepath.Base(path), path); err != nil {
		t.Fatal(err)
	}
	task["status"] = StatusCompleted

	cleaned, err := cleanupTaskReserved(store, task)
	if err == nil {
		t.Fatal("cleanup ignored an inaccessible managed path")
	}
	if cleaned {
		t.Fatal("cleanup reported success for an inaccessible managed path")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("managed path was removed after stat failure: %v", err)
	}
	if !branchExists(repository, branch) {
		t.Fatal("task branch was deleted after stat failure")
	}
}

func TestForbiddenPathInspectionRejectsInvalidCommit(t *testing.T) {
	repository := testRepository(t)
	if _, err := commitTracksForbiddenPaths(repository, "missing-commit"); err == nil {
		t.Fatal("forbidden-path inspection accepted an invalid commit")
	}
}

func TestTaskRebasesOntoAdvancedTarget(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	result := testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "task\n", "feat: add task file")
	target := testCommitFile(t, repository, "target.txt", "target\n", "feat: advance target")

	current := finishTestTask(t, store, task, true)
	if status := stringValue(current, "status"); status != StatusIntegrated {
		t.Fatalf("status = %s, reason = %s", status, stringValue(current, "status_reason"))
	}
	if strategy := stringValue(current, "integration_strategy"); strategy != "rebase" {
		t.Fatalf("strategy = %s", strategy)
	}
	integrated := stringValue(current, "integrated_commit")
	if integrated == result {
		t.Fatal("diverged task result was not rebased")
	}
	parents := strings.Fields(testCommand(t, repository, "git", "rev-list", "--parents", "-n", "1", integrated))
	if len(parents) != 2 || parents[1] != target {
		t.Fatalf("rebased commit parents = %v, want only %s", parents, target)
	}
	if merges := strings.TrimSpace(testCommand(t, repository, "git", "rev-list", "--merges", target+".."+integrated)); merges != "" {
		t.Fatalf("integration created merge commits: %s", merges)
	}
	for _, name := range []string{"task.txt", "target.txt"} {
		if _, err := os.Stat(filepath.Join(repository, name)); err != nil {
			t.Fatalf("rebased target is missing %s: %v", name, err)
		}
	}
}

func TestTaskRebaseConflictPreservesTarget(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	result := testCommitFile(t, stringValue(task, "worktree_path"), "tracked.txt", "task\n", "fix: change task copy")
	target := testCommitFile(t, repository, "tracked.txt", "target\n", "fix: change target copy")

	current := finishTestTask(t, store, task, true)
	if status := stringValue(current, "status"); status != StatusRecovery {
		t.Fatalf("status = %s, want %s", status, StatusRecovery)
	}
	if reason := stringValue(current, "status_reason"); !strings.Contains(reason, "integration conflict") {
		t.Fatalf("unexpected reason: %s", reason)
	}
	if head, _ := gitRef(repository, "refs/heads/main"); head != target {
		t.Fatalf("conflicting integration changed target to %s", head)
	}
	if stringValue(current, "result_commit") != result {
		t.Fatal("conflicting task result was not preserved")
	}
}

func TestTaskRebaseDropsChangesAlreadyOnTarget(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	result := testCommitFile(t, stringValue(task, "worktree_path"), "tracked.txt", "shared\n", "fix: update task copy")
	target := testCommitFile(t, repository, "tracked.txt", "shared\n", "fix: update target copy")

	current := finishTestTask(t, store, task, true)
	if status := stringValue(current, "status"); status != StatusIntegrated {
		t.Fatalf("status = %s, reason = %s", status, stringValue(current, "status_reason"))
	}
	if strategy := stringValue(current, "integration_strategy"); strategy != "redundant" {
		t.Fatalf("strategy = %s", strategy)
	}
	if integrated := stringValue(current, "integrated_commit"); integrated != target {
		t.Fatalf("integrated commit = %s, want unchanged target %s", integrated, target)
	}
	if redundant := stringValue(current, "integration_redundant_result"); redundant != result {
		t.Fatalf("redundant result = %s, want %s", redundant, result)
	}
	if head, _ := gitRef(repository, "refs/heads/main"); head != target {
		t.Fatalf("redundant integration changed target to %s", head)
	}
}

func TestPublishRebasesActiveTaskOntoAdvancedTarget(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	path := stringValue(task, "worktree_path")
	result := testCommitFile(t, path, "task.txt", "task\n", "feat: add task file")
	target := testCommitFile(t, repository, "target.txt", "target\n", "feat: advance target")
	task["status"] = StatusRunning
	task["process"] = processRecord(os.Getpid(), "agent", 0)
	if err := store.Save(task); err != nil {
		t.Fatal(err)
	}

	published, err := publishTaskCheckpoint(store, task)
	if err != nil {
		t.Fatal(err)
	}
	if strategy := stringValue(published, "strategy"); strategy != "rebase" {
		t.Fatalf("strategy = %s", strategy)
	}
	publishedCommit := stringValue(published, "published_commit")
	if publishedCommit == result {
		t.Fatal("published commit was not rebased")
	}
	if head, _ := gitRef(repository, "refs/heads/main"); head != publishedCommit {
		t.Fatalf("main = %s, want %s", head, publishedCommit)
	}
	if head, _ := gitRef(path, "HEAD"); head != publishedCommit {
		t.Fatalf("active task = %s, want %s", head, publishedCommit)
	}
	parents := strings.Fields(testCommand(t, repository, "git", "rev-list", "--parents", "-n", "1", publishedCommit))
	if len(parents) != 2 || parents[1] != target {
		t.Fatalf("published commit parents = %v, want only %s", parents, target)
	}
	changes, err := worktreeChanges(path)
	if err != nil || len(changes.Normal) > 0 {
		t.Fatalf("active task is dirty after publish: %v, %v", changes.Normal, err)
	}
}

func TestTaskRefusesRewoundTarget(t *testing.T) {
	repository := testRepository(t)
	first, _ := gitRef(repository, "HEAD")
	testCommitFile(t, repository, "second.txt", "second\n", "feat: add second base")
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "task\n", "feat: add task result")
	testCommand(t, repository, "git", "update-ref", "refs/heads/main", first)
	testCommand(t, repository, "git", "reset", "--hard", "-q", first)

	current := finishTestTask(t, store, task, true)
	if status := stringValue(current, "status"); status != StatusRecovery {
		t.Fatalf("status = %s, want %s", status, StatusRecovery)
	}
	if reason := stringValue(current, "status_reason"); !strings.Contains(reason, "no longer descends") {
		t.Fatalf("unexpected reason: %s", reason)
	}
	if head, _ := gitRef(repository, "refs/heads/main"); head != first {
		t.Fatalf("rewound target changed to %s", head)
	}
}

func TestValidationMutationBlocksIntegration(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{Checks: []string{"touch validation-marker"}})
	testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "task\n", "feat: add task result")
	targetBefore, _ := gitRef(repository, "refs/heads/main")

	current := finishTestTask(t, store, task, true)
	if status := stringValue(current, "status"); status != StatusRecovery {
		t.Fatalf("status = %s, want %s", status, StatusRecovery)
	}
	failure := recordMap(current, "validation_failure")
	if reason := stringValue(failure, "reason"); !strings.Contains(reason, "changed candidate files") {
		t.Fatalf("unexpected validation failure: %s", describe(failure))
	}
	if head, _ := gitRef(repository, "refs/heads/main"); head != targetBefore {
		t.Fatal("validation mutation advanced the target")
	}
}

func TestValidationDetectsQuotedTerraformPath(t *testing.T) {
	repository := testRepository(t)
	target, err := gitRef(repository, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	path := "directory\nmodule.tf"
	if err := os.WriteFile(filepath.Join(repository, path), []byte("terraform {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testCommand(t, repository, "git", "add", "--", path)
	testCommand(t, repository, "git", "commit", "-q", "-m", "test: add unusual terraform path")
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "terraform"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))

	commands, _, err := validationCommands(Record{"worktree_path": repository, "workdir_relative": "."}, repository, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || len(commands[0]) < 2 || commands[0][0] != "terraform" || commands[0][1] != "fmt" {
		t.Fatalf("Terraform validation was not selected: %#v", commands)
	}
}

func TestValidationStopsWhenStateCannotBeRecorded(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{Checks: []string{"true"}})
	worktree := stringValue(task, "worktree_path")
	head, err := gitRef(worktree, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	path, _ := store.TaskPath(stringValue(task, "task_id"))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	valid, err := validateCandidate(store, task, worktree, head, head)
	if err == nil || valid {
		t.Fatalf("validation continued without durable process state: (%t, %v)", valid, err)
	}
	if processAlive(task["validation_process"]) {
		t.Fatal("validation process survived failed state recording")
	}
}

func TestForbiddenMemoryInIntermediateCommitIsRejected(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	worktree := stringValue(task, "worktree_path")
	testCommand(t, worktree, "git", "add", "-f", MemoryName)
	testCommand(t, worktree, "git", "commit", "-q", "-m", "chore: accidentally track memory")
	testCommand(t, worktree, "git", "rm", "-q", MemoryName)
	testCommand(t, worktree, "git", "commit", "-q", "-m", "fix: remove local memory")
	testCommitFile(t, worktree, "task.txt", "task\n", "feat: add safe result")

	current := finishTestTask(t, store, task, true)
	if status := stringValue(current, "status"); status != StatusRecovery {
		t.Fatalf("status = %s, want %s", status, StatusRecovery)
	}
	if reason := stringValue(current, "status_reason"); !strings.Contains(reason, "forbidden paths") {
		t.Fatalf("unexpected reason: %s", reason)
	}
	if len(recordSlice(current, "forbidden_history")) == 0 {
		t.Fatal("forbidden commit evidence was not recorded")
	}
}

func TestManualIntegrationPolicyLeavesReadyCommit(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{NoIntegrate: true})
	result := testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "task\n", "feat: add manual result")
	targetBefore, _ := gitRef(repository, "refs/heads/main")

	current := finishTestTask(t, store, task, true)
	if status := stringValue(current, "status"); status != StatusReady {
		t.Fatalf("status = %s, want %s", status, StatusReady)
	}
	if stringValue(current, "result_commit") != result {
		t.Fatal("manual result commit was not preserved")
	}
	if head, _ := gitRef(repository, "refs/heads/main"); head != targetBefore {
		t.Fatal("manual integration policy advanced the target")
	}
	if !branchExists(repository, stringValue(task, "branch")) {
		t.Fatal("manual task branch was deleted")
	}
}

func TestAttachmentFinalizationReportsLifecycleError(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	task["attachment_session_id"] = "attachment-session"
	task["status"] = StatusRunning
	task["worktree_path"] = repository
	if err := store.Save(task); err != nil {
		t.Fatal(err)
	}

	results := finalizeSessionAttachments(store, "attachment-session", 0, false)
	if len(results) != 1 || stringValue(results[0], "reason") == "" {
		t.Fatalf("attachment error was not reported: %s", describe(results))
	}
}

func TestAttachmentLeaseRejectsInvalidOwner(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	if err := startAttachmentLease(store, task, Record{"pid": 0}); err == nil {
		t.Fatal("attachment lease accepted an invalid owner")
	}
	identity, err := taskCheckoutIdentity(task)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := store.CheckoutLock(stringValue(task, "worktree_path"), identity, false)
	if err != nil {
		t.Fatalf("invalid attachment owner retained checkout lease: %v", err)
	}
	_ = lock.Unlock()
}

func TestIntegrateCommandReturnsResultInspectionError(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	delete(task, "process")
	task["worktree_path"] = repository
	if err := store.Save(task); err != nil {
		t.Fatal(err)
	}

	code, err := integrateTaskCommand(store, stringValue(task, "task_id"), true)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if err == nil || !strings.Contains(err.Error(), "unexpected managed worktree path") {
		t.Fatalf("unexpected integration error: %v", err)
	}
}
