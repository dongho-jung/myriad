package myriad

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestTaskMergesAdvancedTarget(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "task\n", "feat: add task file")
	testCommitFile(t, repository, "target.txt", "target\n", "feat: advance target")

	current := finishTestTask(t, store, task, true)
	if status := stringValue(current, "status"); status != StatusIntegrated {
		t.Fatalf("status = %s, reason = %s", status, stringValue(current, "status_reason"))
	}
	if strategy := stringValue(current, "integration_strategy"); strategy != "merge" {
		t.Fatalf("strategy = %s", strategy)
	}
	for _, name := range []string{"task.txt", "target.txt"} {
		if _, err := os.Stat(filepath.Join(repository, name)); err != nil {
			t.Fatalf("merged target is missing %s: %v", name, err)
		}
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
