package myriad

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContextCommandDefersTitleWhileCodexTUIIsActive(t *testing.T) {
	store := testStore(t)
	repository := testRepository(t)
	taskID := "deferred-context-task"
	if err := store.Save(Record{
		"task_id": taskID, "status": StatusRunning, "agent": "codex",
		"repository": repository, "worktree_path": repository,
		"branch": "fix-login", "target_branch": "main",
	}); err != nil {
		t.Fatal(err)
	}
	reservation, err := acquireCheckoutSession(store, repository, true, sessionOptions{
		Agent: "codex", TaskID: taskID, Repository: repository,
		WorkingDirectory: repository,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Release(store, repository, "")
	socketDirectory, err := os.MkdirTemp("", "myriad-deferred-title-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, "codex.sock")
	names := testCodexNameServer(t, socketPath)
	if err := updateSessionMetadata(reservation.SessionPath, reservation.SessionID, Record{
		"codex_thread_id": "thread-one", "control_socket": socketPath,
		"notification_ready": true, "notification_state": "ready",
	}); err != nil {
		t.Fatal(err)
	}

	if err := contextCommand(store, taskID, []string{"COM-61"}, nil, false, false); err != nil {
		t.Fatal(err)
	}
	if err := contextCommand(store, taskID, []string{"COM-62"}, nil, false, false); err != nil {
		t.Fatal(err)
	}
	select {
	case name := <-names:
		t.Fatalf("active Codex TUI received premature title %q", name)
	default:
	}

	var session Record
	if err := readJSON(reservation.SessionPath, maxJSONBytes, &session); err != nil {
		t.Fatal(err)
	}
	if got, want := stringValue(session, "codex_thread_name_pending"), "[COM-62] fix-login -> main"; got != want {
		t.Fatalf("pending title = %q, want %q", got, want)
	}
	if err := finalizeCodexThreadName(store, reservation.SessionPath, reservation.SessionID, socketPath, repository, false); err != nil {
		t.Fatal(err)
	}
	if got, want := <-names, "[COM-62] fix-login -> main"; got != want {
		t.Fatalf("finalized title = %q, want %q", got, want)
	}
	if err := readJSON(reservation.SessionPath, maxJSONBytes, &session); err != nil {
		t.Fatal(err)
	}
	if session["codex_thread_name_pending"] != nil {
		t.Fatalf("pending title was not cleared: %#v", session["codex_thread_name_pending"])
	}
}
