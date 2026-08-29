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
	if err := writeTaskContext(store, "primary-task", Record{"jira_issues": []string{"CAPE-123", "COM-42"}}); err != nil {
		t.Fatal(err)
	}
	if err := writeTaskContext(store, "attachment-task", Record{"pull_request_numbers": []int{82, 91}}); err != nil {
		t.Fatal(err)
	}

	if got, want := codexTaskStatusTitle(store, "attachment-task"), "[CAPE-123 COM-42] [#82 #91] fix-login-api -> develop"; got != want {
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

	if err := contextCommand(store, taskID, []string{"cape-123", "com-42", "CAPE-123"}, nil, false, false); err != nil {
		t.Fatal(err)
	}
	if got, want := <-names, "[CAPE-123 COM-42] fix-login -> main"; got != want {
		t.Fatalf("Jira title = %q, want %q", got, want)
	}
	if err := contextCommand(store, taskID, nil, []string{"42", "https://github.com/acme/repo/pull/53", "42"}, false, false); err != nil {
		t.Fatal(err)
	}
	if got, want := <-names, "[CAPE-123 COM-42] [#42 #53] fix-login -> main"; got != want {
		t.Fatalf("PR title = %q, want %q", got, want)
	}
	if err := contextCommand(store, taskID, nil, nil, true, false); err != nil {
		t.Fatal(err)
	}
	if got, want := <-names, "[#42 #53] fix-login -> main"; got != want {
		t.Fatalf("cleared title = %q, want %q", got, want)
	}
	if !strings.HasPrefix(codexManagedStatusLine, `tui.status_line=["thread-title",`) {
		t.Fatalf("managed Codex status line does not lead with its context title: %s", codexManagedStatusLine)
	}
}

func TestRunContextCommandAcceptsSpaceSeparatedValues(t *testing.T) {
	store := testStore(t)
	taskID := "multi-context-task"
	if err := store.Save(Record{
		"task_id": taskID, "status": StatusCreated, "agent": "codex",
		"description": "COM-1 PR #7", "branch": "multi-context", "target_branch": "main",
	}); err != nil {
		t.Fatal(err)
	}

	if err := runContextCommand(store, []string{"--jira", "com-12", "CER-42", "COM-12", "--task", taskID}); err != nil {
		t.Fatal(err)
	}
	if err := runContextCommand(store, []string{"--task", taskID, "--pr", "22", "53,22"}); err != nil {
		t.Fatal(err)
	}
	context := readTaskContext(store, taskID)
	issues, hasIssues, err := taskContextJiraIssues(context)
	if err != nil || !hasIssues {
		t.Fatalf("Jira context = (%v, %t, %v)", issues, hasIssues, err)
	}
	if got, want := strings.Join(issues, " "), "COM-12 CER-42"; got != want {
		t.Fatalf("Jira issues = %q, want %q", got, want)
	}
	numbers, hasNumbers, err := taskContextPullRequestNumbers(context)
	if err != nil || !hasNumbers {
		t.Fatalf("PR context = (%v, %t, %v)", numbers, hasNumbers, err)
	}
	if got, want := displayContext(nil, numbers), "[#22 #53]"; got != want {
		t.Fatalf("pull requests = %q, want %q", got, want)
	}
}

func TestLegacyContextReadsAsPluralAndClearSuppressesInference(t *testing.T) {
	store := testStore(t)
	taskID := "legacy-context-task"
	if err := store.Save(Record{
		"task_id": taskID, "status": StatusCreated,
		"description": "CAPE-999 and PR #999",
	}); err != nil {
		t.Fatal(err)
	}
	payload, err := marshalPrivate(Record{
		"schema_version":      ContextSchema,
		"task_id":             taskID,
		"jira_issue":          "cape-123",
		"pull_request_number": 82,
	})
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.ContextPath(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	issues, numbers := taskDisplayContext(store, Record{
		"task_id": taskID, "description": "CAPE-999 and PR #999",
	})
	if got, want := displayContext(issues, numbers), "[CAPE-123] [#82]"; got != want {
		t.Fatalf("legacy context = %q, want %q", got, want)
	}
	if err := contextCommand(store, taskID, nil, nil, true, false); err != nil {
		t.Fatal(err)
	}
	if err := contextCommand(store, taskID, nil, nil, false, true); err != nil {
		t.Fatal(err)
	}
	issues, numbers = taskDisplayContext(store, Record{
		"task_id": taskID, "description": "CAPE-999 and PR #999",
	})
	if got := displayContext(issues, numbers); got != "" {
		t.Fatalf("cleared context fell back to inferred values: %q", got)
	}
}

func TestTaskDisplayContextInfersMultipleValues(t *testing.T) {
	store := testStore(t)
	issues, numbers := taskDisplayContext(store, Record{
		"task_id":       "inferred-context-task",
		"description":   "COM-12 CER-42, PR #22 and PR #53 and COM-12",
		"source_branch": "feature/CER-42/CAPE-7",
	})
	if got, want := displayContext(issues, numbers), "[COM-12 CER-42 CAPE-7] [#22 #53]"; got != want {
		t.Fatalf("inferred context = %q, want %q", got, want)
	}
}
