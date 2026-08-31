package myriad

import (
	"strings"
	"testing"
)

func TestTaskDiagnosticSummarizesRecoverableState(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	result := testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "task\n", "feat: add diagnostic result")
	task["status"] = StatusValidating
	task["result_commit"] = result
	task["validation_process"] = Record{"pid": 999999999, "role": "validation", "start": "missing"}
	task["memory_base"] = Record{"large": "must not be copied into diagnostics"}
	task["validation_attempts"] = []any{Record{"command": []any{"go", "test", "./..."}, "outcome": "running"}}
	if err := store.Save(task); err != nil {
		t.Fatal(err)
	}

	diagnostic, err := taskDiagnostic(store, stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	summary := recordMap(diagnostic, "task")
	if summary["memory_base"] != nil {
		t.Fatal("diagnostic copied the repository memory snapshot")
	}
	if stringValue(recordMap(diagnostic, "refs"), "target_relation") != targetDescendsBase {
		t.Fatalf("unexpected ref diagnostics: %s", describe(diagnostic["refs"]))
	}
	process := recordMap(recordMap(diagnostic, "processes"), "validation_process")
	if boolValue(process, "alive", true) || boolValue(process, "identity_matches", true) {
		t.Fatalf("dead validation process was reported alive: %s", describe(process))
	}
	if warnings := strings.Join(diagnosticStrings(recordSlice(diagnostic, "warnings")), "\n"); !strings.Contains(warnings, "validation_process") {
		t.Fatalf("diagnostic omitted the dead process warning: %s", warnings)
	}
	worktree := recordMap(diagnostic, "worktree")
	if !boolValue(worktree, "exists", false) || stringValue(worktree, "head") != result {
		t.Fatalf("unexpected worktree diagnostics: %s", describe(worktree))
	}
}

func diagnosticStrings(values []any) []string {
	result := []string{}
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
