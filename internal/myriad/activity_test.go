package myriad

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func testActivitySession(t *testing.T, store *Store, repository, slug string) (Record, *checkoutReservation) {
	t.Helper()
	task := testTask(t, store, repository, createTaskOptions{TaskSlug: slug, Description: "private prompt must not be shared"})
	worktree := stringValue(task, "worktree_path")
	reservation, err := acquireCheckoutSession(store, worktree, true, sessionOptions{
		Agent: "custom", TaskID: stringValue(task, "task_id"), WorkingDirectory: worktree,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := updateSessionMetadata(reservation.SessionPath, reservation.SessionID, Record{
		"process": processRecord(os.Getpid(), "lock-supervisor", 0), "notification_state": "ready",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reservation.Release(store, worktree, "") })
	return task, reservation
}

func testObserveActivity(t *testing.T, store *Store, session *checkoutReservation, intent *activityIntent) string {
	t.Helper()
	context := ""
	err := observeWorkActivity(store, session.SessionPath, session.SessionID, intent, true, func(value string) error {
		context = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return context
}

func TestWorkActivitySharesStartsAndOverlaps(t *testing.T) {
	store, repository := testStore(t), testRepository(t)
	_, first := testActivitySession(t, store, repository, "auth-refresh")
	_, second := testActivitySession(t, store, repository, "auth-tests")
	_, unrelated := testActivitySession(t, store, testRepository(t), "auth-other")
	if context := testObserveActivity(t, store, first, &activityIntent{Summary: "Refresh authentication", Paths: []string{"internal/auth"}}); context != "" {
		t.Fatalf("first session saw an unpublished peer: %s", context)
	}
	context := testObserveActivity(t, store, second, &activityIntent{Summary: "Test token expiry", Paths: []string{"internal/auth/token.go"}})
	if !strings.Contains(context, "Potential overlap") || !strings.Contains(context, "Refresh authentication") || !strings.Contains(context, "internal/auth/token.go") {
		t.Fatalf("new session missed current overlapping work: %s", context)
	}
	if context := testObserveActivity(t, store, first, nil); !strings.Contains(context, "Test token expiry") || !strings.Contains(context, "Potential overlap") {
		t.Fatalf("existing session missed the new peer: %s", context)
	}
	if context := testObserveActivity(t, store, unrelated, nil); context != "" {
		t.Fatalf("activity crossed the repository boundary: %s", context)
	}
	if context := testObserveActivity(t, store, first, nil); context != "" {
		t.Fatalf("unchanged activity was delivered twice: %s", context)
	}
	context = testObserveActivity(t, store, second, &activityIntent{Summary: "Test documentation", Paths: []string{"docs/auth.md"}})
	if strings.Contains(context, "Potential overlap") || !strings.Contains(context, "Refresh authentication") {
		t.Fatalf("changing our own scope did not clear the overlap: %s", context)
	}
	context = testObserveActivity(t, store, first, nil)
	if !strings.Contains(context, "Test documentation") || strings.Contains(context, "Potential overlap") {
		t.Fatalf("changed intent was not propagated: %s", context)
	}
}

func TestWorkActivityDoesNotSharePromptOrStaleSessions(t *testing.T) {
	store, repository := testStore(t), testRepository(t)
	_, first := testActivitySession(t, store, repository, "first-work")
	secondTask, second := testActivitySession(t, store, repository, "second-work")
	testObserveActivity(t, store, first, nil)
	context := testObserveActivity(t, store, second, nil)
	if strings.Contains(context, "private prompt") || !strings.Contains(context, "first-work") {
		t.Fatalf("fallback activity exposed a prompt or lost its branch: %s", context)
	}
	testObserveActivity(t, store, first, nil)
	second.Release(store, stringValue(secondTask, "worktree_path"), "")
	if context := testObserveActivity(t, store, first, nil); context != "" {
		t.Fatalf("closed peer was still announced: %s", context)
	}
	inbox, err := readSessionInbox(store, first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recordSlice(inbox, "messages")) != 0 {
		t.Fatalf("closed peer retained a current activity notice: %#v", inbox)
	}
}

func TestActivityChangedPathsIncludeCommitsAndLiteralNames(t *testing.T) {
	store, repository := testStore(t), testRepository(t)
	testCommitFile(t, repository, "partially-staged.txt", "base\n", "test: add partially staged fixture")
	task := testTask(t, store, repository, createTaskOptions{})
	worktree := stringValue(task, "worktree_path")
	testCommand(t, worktree, "git", "mv", "tracked.txt", "renamed file.txt")
	testCommand(t, worktree, "git", "commit", "-qm", "fix: rename file")
	for _, name := range []string{"new\n한국어.txt", "staged.txt", MemoryName, "ignored.log"} {
		if err := os.WriteFile(filepath.Join(worktree, name), []byte("private contents\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	testCommand(t, worktree, "git", "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(worktree, "partially-staged.txt"), []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testCommand(t, worktree, "git", "add", "partially-staged.txt")
	if err := os.WriteFile(filepath.Join(worktree, "partially-staged.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	common := stringValue(task, "git_common_dir")
	if err := os.WriteFile(filepath.Join(common, "info", "exclude"), []byte(MemoryName+"\nignored.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, truncated, err := activityChangedPaths(task)
	if err != nil || truncated {
		t.Fatalf("changed paths: %v, truncated=%t", err, truncated)
	}
	want := []string{"new\n한국어.txt", "partially-staged.txt", "renamed file.txt", "staged.txt", "tracked.txt"}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestWorkActivityFindsAttachedRepositoryPeers(t *testing.T) {
	store, primary, secondary := testStore(t), testRepository(t), testRepository(t)
	parent, first := testActivitySession(t, store, primary, "primary-work")
	_, peer := testActivitySession(t, store, secondary, "secondary-work")
	attachment := testTask(t, store, secondary, createTaskOptions{TaskSlug: "attached-work"})
	attachment["attachment_session_id"] = first.SessionID
	attachment["attachment_parent_task_id"] = parent["task_id"]
	if err := store.Save(attachment); err != nil {
		t.Fatal(err)
	}
	testObserveActivity(t, store, first, nil)
	context := testObserveActivity(t, store, peer, nil)
	if !strings.Contains(context, "attached-work") || strings.Contains(context, "primary-work") {
		t.Fatalf("attachment routing leaked or missed a repository: %s", context)
	}
	context = testObserveActivity(t, store, first, nil)
	if !strings.Contains(context, "secondary-work") {
		t.Fatalf("attached session missed its secondary peer: %s", context)
	}
}

func TestConcurrentWorkHooksDeliverOnce(t *testing.T) {
	store, repository := testStore(t), testRepository(t)
	_, first := testActivitySession(t, store, repository, "first-work")
	_, second := testActivitySession(t, store, repository, "second-work")
	testObserveActivity(t, store, first, nil)
	results := make(chan string, 8)
	errors := make(chan error, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			context := ""
			err := observeWorkActivity(store, second.SessionPath, second.SessionID, nil, true, func(value string) error {
				context = value
				return nil
			})
			results <- context
			errors <- err
		}()
	}
	workers.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	delivered := 0
	for context := range results {
		if context != "" {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("concurrent hooks delivered the same event %d times", delivered)
	}
}

func TestWorkActivityRetriesFailedOutput(t *testing.T) {
	store, repository := testStore(t), testRepository(t)
	_, first := testActivitySession(t, store, repository, "first-work")
	_, second := testActivitySession(t, store, repository, "second-work")
	testObserveActivity(t, store, first, nil)
	outputError := errors.New("output pipe closed")
	err := observeWorkActivity(store, second.SessionPath, second.SessionID, nil, true, func(string) error {
		return outputError
	})
	if !errors.Is(err, outputError) {
		t.Fatalf("delivery failure = %v, want output error", err)
	}
	pending, err := pendingInboxMessages(store, second.SessionID, false)
	if err != nil || len(pending) != 1 {
		t.Fatalf("failed delivery lost its pending notice: %#v, %v", pending, err)
	}
	if context := testObserveActivity(t, store, second, nil); !strings.Contains(context, "first-work") {
		t.Fatalf("failed output could not be retried: %s", context)
	}
	if context := testObserveActivity(t, store, second, nil); context != "" {
		t.Fatalf("successful retry was delivered again: %s", context)
	}
}

func TestActivityHookCLIProvidesNonBlockingContext(t *testing.T) {
	myriad, _ := testMyriadBinaries(t)
	store, repository := testStore(t), testRepository(t)
	_, first := testActivitySession(t, store, repository, "source-work")
	task, second := testActivitySession(t, store, repository, "receiver-work")
	testObserveActivity(t, store, first, &activityIntent{Summary: "Change login flow", Paths: []string{"auth"}})
	environment := overlayEnvironment(taskEnvironment(task), map[string]string{
		envAgentSessionID: second.SessionID, envAgentSessionPath: second.SessionPath,
	})
	runHook := func(event string) string {
		t.Helper()
		command := exec.Command(myriad, internalActivityHook)
		command.Dir, command.Env = stringValue(task, "worktree_path"), environment
		command.Stdin = strings.NewReader(`{"hook_event_name":"` + event + `"}`)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("activity hook failed: %v\n%s", err, output)
		}
		return string(output)
	}
	if output := runHook("Stop"); output != "" {
		t.Fatalf("advisory notice interfered with stopping: %s", output)
	}
	if err := deliverPendingCodexNotifications(store, second.SessionID, filepath.Join(t.TempDir(), "absent.sock"), repository); err != nil {
		t.Fatalf("work notice attempted to wake Codex: %v", err)
	}
	output := runHook("PostToolUse")
	value := Record{}
	if err := json.Unmarshal([]byte(output), &value); err != nil {
		t.Fatalf("hook did not return JSON: %v, %s", err, output)
	}
	if value["decision"] != nil || value["continue"] != nil || !strings.Contains(stringValue(recordMap(value, "hookSpecificOutput"), "additionalContext"), "Change login flow") {
		t.Fatalf("hook blocked a tool or lost the notice: %s", output)
	}
	if output := runHook("PostToolUse"); output != "" {
		t.Fatalf("hook repeated a delivered notice: %s", output)
	}
	if output := runHook("UserPromptSubmit"); !strings.Contains(output, "myriad activity --summary") {
		t.Fatalf("new prompt did not explain automatic intent sharing: %s", output)
	}
	command := exec.Command(myriad, "activity", "--summary", "Test auth tokens", "--path", "auth/token.go")
	command.Dir, command.Env = stringValue(task, "worktree_path"), environment
	if output, err := command.CombinedOutput(); err != nil || !strings.Contains(string(output), "Potential overlap") {
		t.Fatalf("activity CLI did not identify intended overlap: %v\n%s", err, output)
	}
	if context := testObserveActivity(t, store, first, nil); !strings.Contains(context, "Test auth tokens") {
		t.Fatalf("CLI intent did not reach the other session: %s", context)
	}
	command = exec.Command(myriad, internalActivityHook)
	command.Dir, command.Env = stringValue(task, "worktree_path"), environment
	command.Stdin = strings.NewReader("invalid hook input")
	if output, err := command.CombinedOutput(); err != nil || !strings.Contains(string(output), "work activity input unavailable") {
		t.Fatalf("malformed advisory hook input blocked a tool: %v\n%s", err, output)
	}
}

func TestWorkActivityDetectsFileOverlapWithoutIntentUpdate(t *testing.T) {
	store, repository := testStore(t), testRepository(t)
	_, first := testActivitySession(t, store, repository, "review-tests")
	task, second := testActivitySession(t, store, repository, "update-docs")
	testObserveActivity(t, store, first, &activityIntent{Summary: "Review tests", Paths: []string{"tracked.txt"}})
	testObserveActivity(t, store, second, nil)
	if context := testObserveActivity(t, store, first, nil); strings.Contains(context, "Potential overlap") {
		t.Fatalf("unmodified peer had a false overlap: %s", context)
	}
	testCommitFile(t, stringValue(task, "worktree_path"), "tracked.txt", "changed\n", "fix: update shared fixture")
	testObserveActivity(t, store, second, nil)
	if context := testObserveActivity(t, store, first, nil); !strings.Contains(context, "Potential overlap") || !strings.Contains(context, "tracked.txt") {
		t.Fatalf("an undeclared committed edit was not propagated: %s", context)
	}
}

func TestActivityPathsStayWithinTheirRepository(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"../escape", "/another/repository", "", "null\x00path"} {
		if _, err := normalizeActivityPaths(root, []string{path}); err == nil {
			t.Fatalf("accepted invalid activity path %q", path)
		}
	}
	paths, err := normalizeActivityPaths(root, []string{"internal/../auth", "auth", filepath.Join(root, "auth/token.go")})
	if err != nil || !slices.Equal(paths, []string{"auth", "auth/token.go"}) {
		t.Fatalf("normalized paths = %#v, %v", paths, err)
	}
	a := Record{"intent_paths": []any{"auth"}}
	b := Record{"intent_paths": []any{"authorization/file.go"}}
	if paths := activityOverlap(a, b); len(paths) != 0 {
		t.Fatalf("directory prefix produced a false overlap: %#v", paths)
	}
}

func TestClaudeActivityHooksPreserveSettingsAndArguments(t *testing.T) {
	settings := `{"env":{"KEEP":"yes"},"hooks":{"PostToolUse":[{"hooks":[{"type":"command","command":"existing-hook"}]}]}}`
	for _, value := range []string{settings, filepath.Join(t.TempDir(), "settings.json")} {
		if value != settings {
			if err := os.WriteFile(value, []byte(settings), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		command, err := claudeActivityCommand([]string{"env", "IS_DEMO=1", "claude", "--settings", value, "--", "a prompt"}, "/path/with a 'quote/myriad")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(command[:4], []string{"env", "IS_DEMO=1", "claude", "--settings"}) || !slices.Equal(command[len(command)-2:], []string{"--", "a prompt"}) {
			t.Fatalf("hook options changed the command: %#v", command)
		}
		parsed := Record{}
		if err := json.Unmarshal([]byte(command[4]), &parsed); err != nil {
			t.Fatal(err)
		}
		if stringValue(recordMap(parsed, "env"), "KEEP") != "yes" || len(recordSlice(recordMap(parsed, "hooks"), "PostToolUse")) != 2 {
			t.Fatalf("caller settings or hooks were overwritten: %s", command[4])
		}
	}
}
