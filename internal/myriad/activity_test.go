package myriad

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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
	if intent != nil {
		metadata := Record{}
		if err := readJSON(session.SessionPath, maxJSONBytes, &metadata); err != nil {
			t.Fatal(err)
		}
		copy := *intent
		copy.TaskID = stringValue(metadata, "task_id")
		intent = &copy
	}
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

func TestActivityCommandRequiresTheOwningWorktree(t *testing.T) {
	store, repository := testStore(t), testRepository(t)
	parent, session := testActivitySession(t, store, repository, "primary-work")
	attachment := testTask(t, store, testRepository(t), createTaskOptions{TaskSlug: "attached-work"})
	attachment["attachment_session_id"] = session.SessionID
	attachment["attachment_parent_task_id"] = parent["task_id"]
	if err := store.Save(attachment); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envAgentSessionID, session.SessionID)
	t.Setenv(envAgentSessionPath, session.SessionPath)
	t.Chdir(repository)
	if err := activityCommand(store, []string{"--summary", "Wrong checkout"}); err == nil {
		t.Fatal("activity accepted the original checkout instead of a managed worktree")
	}
	t.Chdir(stringValue(attachment, "worktree_path"))
	if err := activityCommand(store, []string{"--summary", "Attached scope", "--path", "tracked.txt"}); err != nil {
		t.Fatal(err)
	}
	metadata := Record{}
	if err := readJSON(session.SessionPath, maxJSONBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	activities := recordMap(metadata, "work_activity")
	if stringValue(recordMap(activities, stringValue(attachment, "task_id")), "summary") != "Attached scope" {
		t.Fatal("activity did not select the attached repository")
	}
	if stringValue(recordMap(activities, stringValue(parent, "task_id")), "summary") != "primary-work" {
		t.Fatal("attached activity overwrote the primary repository's scope")
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

func TestWorkActivityThrottlesScansFromTheirCompletion(t *testing.T) {
	store, repository := testStore(t), testRepository(t)
	task, session := testActivitySession(t, store, repository, "throttled-work")
	started := time.Now()
	metadata, err := refreshWorkActivity(store, session.SessionPath, session.SessionID, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	checked, err := time.Parse(time.RFC3339Nano, stringValue(metadata, "work_activity_checked_at"))
	if err != nil || checked.Before(started) || checked.After(time.Now()) {
		t.Fatalf("scan completion lost precision: %s, %v", checked, err)
	}
	if err := os.WriteFile(filepath.Join(stringValue(task, "worktree_path"), "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, err = refreshWorkActivity(store, session.SessionPath, session.SessionID, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	activity := recordMap(recordMap(metadata, "work_activity"), stringValue(task, "task_id"))
	if len(activityStrings(activity, "changed_paths")) != 0 {
		t.Fatal("tool hook rescanned before the throttle interval elapsed")
	}
	if err := updateSessionMetadata(session.SessionPath, session.SessionID, Record{
		"work_activity_checked_at": time.Now().Add(-activityScanInterval).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	metadata, err = refreshWorkActivity(store, session.SessionPath, session.SessionID, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	activity = recordMap(recordMap(metadata, "work_activity"), stringValue(task, "task_id"))
	if !slices.Equal(activityStrings(activity, "changed_paths"), []string{"tracked.txt"}) {
		t.Fatalf("tool hook did not refresh after the interval: %#v", activity)
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
	if paths, _ := activityOverlap(a, b); len(paths) != 0 {
		t.Fatalf("directory prefix produced a false overlap: %#v", paths)
	}
}

func TestWorkActivityReportsIncompleteScopes(t *testing.T) {
	paths := make([]string, maxActivityPaths)
	for index := range paths {
		paths[index] = fmt.Sprintf("auth/%03d.go", index)
	}
	peer := Record{"session_id": "peer", "agent": "custom"}
	activity := Record{
		"task_id": "peer-task", "intent_paths": []any{"auth/extra.go"}, "changed_paths": stringsToAny(paths),
	}
	own := Record{"intent_paths": []any{"auth"}}
	notice := workActivityNotice("receiver", peer, activity, []Record{own})
	if len(activityStrings(notice, "overlap_paths")) != maxActivityPaths || !boolValue(notice, "paths_truncated", false) {
		t.Fatalf("bounded overlap was presented as complete: %#v", notice)
	}
	activity["changed_paths"] = []any{}
	own["paths_truncated"] = true
	notice = workActivityNotice("receiver", peer, activity, []Record{own})
	if !boolValue(notice, "paths_truncated", false) {
		t.Fatal("notice ignored missing paths from the receiver's scope")
	}
	own["paths_truncated"] = false
	activity["paths_truncated"] = true
	notice = workActivityNotice("receiver", peer, activity, []Record{own})
	if !boolValue(notice, "paths_truncated", false) {
		t.Fatal("notice ignored missing paths from the peer's scope")
	}
	activity["paths_truncated"] = false
	activity["changed_paths"] = stringsToAny(paths[:maxActivityNoticePaths+1])
	notice = workActivityNotice("receiver", peer, activity, []Record{own})
	_, payload, found := strings.Cut(stringValue(notice, "prompt"), "\n")
	preview := Record{}
	if !found || decodeJSON([]byte(payload), &preview) != nil {
		t.Fatal("notice has no JSON preview")
	}
	if !boolValue(preview, "paths_truncated", false) || len(activityStrings(preview, "changed_paths")) != maxActivityNoticePaths {
		t.Fatalf("shortened preview was presented as complete: %#v", preview)
	}
	if count, _ := intValue(preview["changed_paths_count"]); count != maxActivityNoticePaths+1 {
		t.Fatalf("preview lost its stored path count: %#v", preview)
	}
}

func TestWorkActivityCombinesOwnedRepositoryScopes(t *testing.T) {
	peer := Record{"session_id": "peer", "agent": "custom"}
	activity := Record{"task_id": "peer-task", "intent_paths": []any{"auth"}}
	own := []Record{
		{"intent_paths": []any{"auth/login.go"}},
		{"intent_paths": []any{"auth/refresh.go"}},
	}
	first := workActivityNotice("receiver", peer, activity, own)
	slices.Reverse(own)
	second := workActivityNotice("receiver", peer, activity, own)
	if !slices.Equal(activityStrings(first, "overlap_paths"), []string{"auth/login.go", "auth/refresh.go"}) {
		t.Fatalf("notice lost an owned repository scope: %#v", first)
	}
	if first["fingerprint"] != second["fingerprint"] {
		t.Fatal("metadata iteration order changed the notice fingerprint")
	}
}

func TestClaudeActivityHooksPreserveSettingsAndArguments(t *testing.T) {
	for _, arguments := range [][]string{
		{"--settings", "/a symlink/settings.json", "--", "a prompt"},
		{"--append-system-prompt", "--settings", "--settings={\"env\":{\"KEEP\":\"yes\"}}", "prompt"},
		{"--plugin-dir", "/another/plugin", "--settings", "{\"hooks\":{}}"},
	} {
		original := append([]string{"env", "IS_DEMO=1", "claude"}, arguments...)
		command := claudeActivityCommand(original, "/pinned/plugin")
		if !slices.Equal(command[:5], []string{"env", "IS_DEMO=1", "claude", "--plugin-dir", "/pinned/plugin"}) || !slices.Equal(command[5:], arguments) {
			t.Fatalf("hook integration rewrote caller arguments: %#v", command)
		}
		if !slices.Equal(original[3:], arguments) {
			t.Fatal("hook integration mutated its input command")
		}
	}
}

func TestClaudeActivityPluginIsPinnedAndValid(t *testing.T) {
	store := testStore(t)
	root, err := materializeClaudeActivityPlugin(store)
	if err != nil {
		t.Fatal(err)
	}
	if claude, err := exec.LookPath("claude"); err == nil {
		command := exec.Command(claude, "plugin", "validate", root)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("Claude rejected its activity plugin: %v\n%s", err, output)
		}
	}
	path := filepath.Join(root, "hooks", "hooks.json")
	hooks := Record{}
	if err := readJSON(path, maxJSONBytes, &hooks); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"UserPromptSubmit", "PostToolUse", "Stop"} {
		entries := recordSlice(recordMap(hooks, "hooks"), event)
		if len(entries) != 1 {
			t.Fatalf("plugin is missing %s", event)
		}
		command := anyRecord(recordSlice(anyRecord(entries[0]), "hooks")[0])
		if stringValue(command, "command") != hookCommand(internalActivityHook, filepath.Join(root, "myriad")) {
			t.Fatalf("plugin hook does not use its pinned binary: %#v", command)
		}
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := materializeClaudeActivityPlugin(store); err == nil {
		t.Fatal("tampered plugin hooks were silently replaced")
	}
}
