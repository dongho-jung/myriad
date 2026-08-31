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
