package myriad

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func testCodexNameServer(t *testing.T, socketPath string) <-chan string {
	t.Helper()
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	names := make(chan string, 8)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, upgradeErr := upgrader.Upgrade(writer, request, nil)
		if upgradeErr != nil {
			return
		}
		defer connection.Close()
		for {
			message := Record{}
			if readErr := connection.ReadJSON(&message); readErr != nil {
				return
			}
			requestID, hasID := intValue(message["id"])
			switch stringValue(message, "method") {
			case "initialize":
				if hasID {
					_ = connection.WriteJSON(Record{"id": requestID, "result": Record{}})
				}
			case "thread/name/set":
				params := anyRecord(message["params"])
				names <- stringValue(params, "name")
				if hasID {
					_ = connection.WriteJSON(Record{"id": requestID, "result": Record{}})
				}
			}
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})
	return names
}

func TestCodexTaskStatusTitleUsesAttachmentAndPrimaryContext(t *testing.T) {
	store := testStore(t)
	primary := Record{
		"task_id": "primary-task", "status": StatusRunning,
		"branch": "fix-login", "target_branch": "main",
	}
	attachment := Record{
		"task_id": "attachment-task", "status": StatusRunning,
		"branch": "fix-login-api", "target_branch": "develop",
		"attachment_parent_task_id": "primary-task",
	}
	if err := store.Save(primary); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(attachment); err != nil {
		t.Fatal(err)
	}
	if err := writeTaskContext(store, "primary-task", Record{"jira_issue": "CAPE-123"}); err != nil {
		t.Fatal(err)
	}
	if err := writeTaskContext(store, "attachment-task", Record{"pull_request_number": 82}); err != nil {
		t.Fatal(err)
	}

	if got, want := codexTaskStatusTitle(store, "attachment-task"), "[CAPE-123|PR#82] fix-login-api -> develop"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
}

func TestContextCommandRefreshesActiveCodexTitle(t *testing.T) {
	store := testStore(t)
	repository := testRepository(t)
	taskID := "context-task"
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
	socketDirectory, err := os.MkdirTemp("", "myriad-statusline-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, "codex.sock")
	names := testCodexNameServer(t, socketPath)
	if err := updateSessionMetadata(reservation.SessionPath, reservation.SessionID, Record{
		"codex_thread_id": "thread-one", "control_socket": socketPath,
	}); err != nil {
		t.Fatal(err)
	}

	if err := contextCommand(store, taskID, "cape-123", "", false, false); err != nil {
		t.Fatal(err)
	}
	if got, want := <-names, "[CAPE-123] fix-login -> main"; got != want {
		t.Fatalf("Jira title = %q, want %q", got, want)
	}
	if err := contextCommand(store, taskID, "", "42", false, false); err != nil {
		t.Fatal(err)
	}
	if got, want := <-names, "[CAPE-123|PR#42] fix-login -> main"; got != want {
		t.Fatalf("PR title = %q, want %q", got, want)
	}
	if err := contextCommand(store, taskID, "", "", true, false); err != nil {
		t.Fatal(err)
	}
	if got, want := <-names, "[PR#42] fix-login -> main"; got != want {
		t.Fatalf("cleared title = %q, want %q", got, want)
	}
	if !strings.HasPrefix(codexManagedStatusLine, `tui.status_line=["thread-title",`) {
		t.Fatalf("managed Codex status line does not lead with its context title: %s", codexManagedStatusLine)
	}
}
