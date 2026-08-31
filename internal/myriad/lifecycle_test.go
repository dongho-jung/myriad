package myriad

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestRecreateWorktreeClearsCleanedMarker(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	path := stringValue(task, "worktree_path")
	if _, err := gitCommand(repository, true, "worktree", "unlock", path); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(repository, true, "worktree", "remove", path); err != nil {
		t.Fatal(err)
	}
	task["worktree_cleaned_at"] = now()
	if err := store.Save(task); err != nil {
		t.Fatal(err)
	}

	if err := recreateWorktree(store, task); err != nil {
		t.Fatal(err)
	}
	current, err := store.Load(stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(current, "worktree_cleaned_at") != "" {
		t.Fatal("recreated worktree is still marked as cleaned")
	}
	if stringValue(current, "worktree_recreated_at") == "" {
		t.Fatal("worktree recreation was not recorded")
	}
	if registered, err := worktreeRegistered(repository, path); err != nil || !registered {
		t.Fatalf("recreated worktree registered = %t, error = %v", registered, err)
	}
}

func TestReconcileCompletesEmptyInterruptedTasks(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		name := "provisioned"
		if deferred {
			name = "reserved"
		}
		t.Run(name, func(t *testing.T) {
			repository := testRepository(t)
			store := testStore(t)
			task, err := createTask(store, createTaskOptions{
				Agent:       "custom",
				Deferred:    deferred,
				Description: "empty interrupted task",
				LaunchCWD:   repository,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !deferred {
				task["status"] = StatusRunning
			}
			delete(task, "process")
			if err := store.Save(task); err != nil {
				t.Fatal(err)
			}

			if code := reconcile(store, false, true); code != 0 {
				t.Fatalf("reconcile exit code = %d, want 0", code)
			}
			current, err := store.Load(stringValue(task, "task_id"))
			if err != nil {
				t.Fatal(err)
			}
			if status := stringValue(current, "status"); status != StatusCompleted {
				t.Fatalf("status = %s, reason = %s", status, stringValue(current, "status_reason"))
			}
			if stringValue(current, "empty_interruption_resolved_at") == "" {
				t.Fatal("reconcile did not record automatic empty-task resolution")
			}
			if _, err := os.Stat(stringValue(task, "worktree_path")); !os.IsNotExist(err) {
				t.Fatalf("empty interrupted worktree still exists: %v", err)
			}
		})
	}
}

func TestReconcilePreservesInterruptedCommit(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	result := testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "preserved\n", "fix: preserve interrupted result")
	task["status"] = StatusRunning
	delete(task, "process")
	if err := store.Save(task); err != nil {
		t.Fatal(err)
	}

	if code := reconcile(store, false, true); code != 2 {
		t.Fatalf("reconcile exit code = %d, want 2 for preserved recovery", code)
	}
	current, err := store.Load(stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	if status := stringValue(current, "status"); status != StatusRecovery {
		t.Fatalf("status = %s, want %s", status, StatusRecovery)
	}
	if got := stringValue(current, "result_commit"); got != result {
		t.Fatalf("preserved result = %s, want %s", got, result)
	}
	if _, err := os.Stat(stringValue(task, "worktree_path")); err != nil {
		t.Fatalf("interrupted worktree was not preserved: %v", err)
	}
}

func TestQueuedIntegrationRecordsSessionBlocker(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "task\n", "feat: add queued result")
	recordAgentExit(task, 0, false)
	if err := inspectResult(store, task, false); err != nil {
		t.Fatal(err)
	}

	reservation, err := acquireCheckoutSession(store, repository, true, sessionOptions{Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Release(store, repository, "")
	owner := processRecord(os.Getpid(), "lock-supervisor", 0)
	if err := updateSessionMetadata(reservation.SessionPath, reservation.SessionID, Record{
		"process": owner, "notification_state": "ready", "notification_ready": true,
	}); err != nil {
		t.Fatal(err)
	}

	current, err := store.Load(stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	if integrated, err := integrateTask(store, current); err != nil || integrated {
		t.Fatalf("blocked integration = (%t, %v), want queued", integrated, err)
	}
	queued, err := store.Load(stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := recordMap(queued, "last_integration_diagnostic")
	if stringValue(diagnostic, "outcome") != "queued" || !strings.Contains(stringValue(diagnostic, "reason"), "active agent") {
		t.Fatalf("unexpected integration diagnostic: %s", describe(diagnostic))
	}
	blockers := recordSlice(diagnostic, "blockers")
	if len(blockers) != 1 || stringValue(anyRecord(blockers[0]), "session_id") != reservation.SessionID {
		t.Fatalf("session blocker was not retained: %s", describe(blockers))
	}
	if detail := integrationRetryFailure(store, stringValue(task, "task_id"), 2, nil); strings.Contains(detail, "<nil>") || !strings.Contains(detail, "active agent") {
		t.Fatalf("retry detail lost the queue reason: %q", detail)
	}
}

func TestLifecycleHistoryIsBounded(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	for index := 0; index < lifecycleHistory+10; index++ {
		status := StatusRunning
		if index%2 == 0 {
			status = StatusReady
		}
		if err := setStatus(store, task, status, fmt.Sprintf("transition %d", index)); err != nil {
			t.Fatal(err)
		}
	}
	history := recordSlice(task, "lifecycle_history")
	if len(history) != lifecycleHistory {
		t.Fatalf("lifecycle history length = %d, want %d", len(history), lifecycleHistory)
	}
	if got := stringValue(anyRecord(history[len(history)-1]), "reason"); got != "transition 73" {
		t.Fatalf("latest lifecycle transition = %q", got)
	}
}

func TestConcurrentTaskCreationIsSerialized(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	const taskCount = 12
	type creation struct {
		task Record
		err  error
	}
	start := make(chan struct{})
	results := make(chan creation, taskCount)
	var group sync.WaitGroup
	for index := 0; index < taskCount; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			task, err := createTask(store, createTaskOptions{
				Agent:       "custom",
				Description: "concurrent lifecycle task",
				LaunchCWD:   repository,
			})
			results <- creation{task: task, err: err}
		}()
	}
	close(start)
	group.Wait()
	close(results)

	tasks := make([]Record, 0, taskCount)
	ids := map[string]bool{}
	numbers := map[int]bool{}
	branches := map[string]bool{}
	paths := map[string]bool{}
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent task creation failed: %v", result.err)
		}
		task := result.task
		if !taskWorktreeReady(task) {
			t.Fatalf("concurrent task was not provisioned: %s", describe(task))
		}
		id := stringValue(task, "task_id")
		number, ok := intValue(task["worktree_number"])
		if !ok || number <= 0 {
			t.Fatalf("task has invalid worktree number: %s", describe(task))
		}
		branch := stringValue(task, "branch")
		path := stringValue(task, "worktree_path")
		if ids[id] || numbers[number] || branches[branch] || paths[path] {
			t.Fatalf("concurrent allocation collided: %s", describe(task))
		}
		ids[id], numbers[number], branches[branch], paths[path] = true, true, true, true
		tasks = append(tasks, task)
	}
	if len(tasks) != taskCount {
		t.Fatalf("created %d tasks, want %d", len(tasks), taskCount)
	}
	for _, task := range tasks {
		current := finishTestTask(t, store, task, false)
		if status := stringValue(current, "status"); status != StatusCompleted {
			t.Fatalf("concurrent task cleanup status = %s", status)
		}
	}
}

func TestReconcileRetriesInterruptedIntegration(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	result := testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "result\n", "feat: add interrupted result")
	targetBefore, err := gitRef(repository, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	key, err := repoKey(repository)
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(store.Integrations, key, stringValue(task, "task_id"))
	if err := os.MkdirAll(filepath.Dir(candidate), 0o700); err != nil {
		t.Fatal(err)
	}
	testCommand(t, repository, "git", "worktree", "add", "--detach", "-q", candidate, result)
	task["result_commit"] = result
	task["integration_candidate"] = candidate
	task["status"] = StatusValidating
	delete(task, "process")
	if err := store.Save(task); err != nil {
		t.Fatal(err)
	}

	if code := reconcile(store, false, true); code != 0 {
		t.Fatalf("non-integrating reconcile exit code = %d, want 0", code)
	}
	queued, err := store.Load(stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	if status := stringValue(queued, "status"); status != StatusReady {
		t.Fatalf("interrupted integration status = %s, want %s", status, StatusReady)
	}
	if stringValue(queued, "integration_candidate") != "" || queued["integration_process"] != nil || queued["validation_process"] != nil {
		t.Fatalf("interrupted integration state was not cleared: %s", describe(queued))
	}
	if _, err := os.Stat(candidate); !os.IsNotExist(err) {
		t.Fatalf("stale integration candidate still exists: %v", err)
	}
	if target, _ := gitRef(repository, "refs/heads/main"); target != targetBefore {
		t.Fatalf("non-integrating reconcile changed target: %s -> %s", targetBefore, target)
	}

	if code := reconcile(store, true, true); code != 0 {
		t.Fatalf("integration retry exit code = %d, want 0", code)
	}
	current, err := store.Load(stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	if status := stringValue(current, "status"); status != StatusIntegrated {
		t.Fatalf("retried integration status = %s, reason = %s", status, stringValue(current, "status_reason"))
	}
	if target, _ := gitRef(repository, "refs/heads/main"); target != result {
		t.Fatalf("retried integration target = %s, want %s", target, result)
	}
}

func TestReconcileRecognizesInterruptedTargetAdvance(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	result := testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "result\n", "feat: add advanced result")
	testCommand(t, repository, "git", "merge", "--ff-only", "-q", result)
	key, err := repoKey(repository)
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(store.Integrations, key, stringValue(task, "task_id"))
	if err := os.MkdirAll(filepath.Dir(candidate), 0o700); err != nil {
		t.Fatal(err)
	}
	testCommand(t, repository, "git", "worktree", "add", "--detach", "-q", candidate, result)
	task["result_commit"] = result
	task["integration_candidate"] = candidate
	task["status"] = StatusIntegrating
	delete(task, "process")
	if err := store.Save(task); err != nil {
		t.Fatal(err)
	}

	if code := reconcile(store, true, true); code != 0 {
		t.Fatalf("reconcile exit code = %d, want 0", code)
	}
	current, err := store.Load(stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	if status := stringValue(current, "status"); status != StatusIntegrated {
		t.Fatalf("recognized integration status = %s, reason = %s", status, stringValue(current, "status_reason"))
	}
	if got := stringValue(current, "integrated_commit"); got != result {
		t.Fatalf("recognized integrated commit = %s, want %s", got, result)
	}
	for _, path := range []string{candidate, stringValue(task, "worktree_path")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("reconciled worktree still exists at %s: %v", path, err)
		}
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

func TestListedWorktreesPreservesUnusualPaths(t *testing.T) {
	repository := testRepository(t)
	path := filepath.Join(t.TempDir(), "worktree\nline")
	testCommand(t, repository, "git", "worktree", "add", "-q", "-b", "unusual-worktree", path)

	records, err := listedWorktrees(repository)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := canonical(path)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range records {
		candidate, candidateErr := canonical(record["worktree"])
		if candidateErr == nil && candidate == resolved {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("unusual worktree path %q was not parsed: %#v", resolved, records)
	}
	registered, err := worktreeRegistered(repository, path)
	if err != nil {
		t.Fatal(err)
	}
	if !registered {
		t.Fatal("unusual worktree path was not recognized as registered")
	}
	checkout, err := targetCheckout(repository, "unusual-worktree")
	if err != nil {
		t.Fatal(err)
	}
	if checkout != resolved {
		t.Fatalf("target checkout = %q, want %q", checkout, resolved)
	}
}

func TestGitCommandsIgnoreInheritedRepositorySelection(t *testing.T) {
	repository := testRepository(t)
	other := testRepository(t)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)

	root, err := repoRoot(repository)
	if err != nil {
		t.Fatal(err)
	}
	want, err := canonical(repository)
	if err != nil {
		t.Fatal(err)
	}
	if root != want {
		t.Fatalf("repository root = %q, want %q", root, want)
	}
}

func TestTargetCheckoutRejectsDuplicateBranch(t *testing.T) {
	repository := testRepository(t)
	duplicate := filepath.Join(t.TempDir(), "duplicate-main")
	testCommand(t, repository, "git", "worktree", "add", "--force", "-q", duplicate, "main")

	if _, err := targetCheckout(repository, "main"); err == nil {
		t.Fatal("duplicate target branch checkouts were accepted")
	}
}

func TestRecordedOrphanRemainsManageable(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	path := stringValue(task, "worktree_path")
	registry, err := store.TaskPath(stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(registry); err != nil {
		t.Fatal(err)
	}

	recordOrphans(store)
	var orphan Record
	for _, candidate := range store.All(true) {
		if boolValue(candidate, "orphan_discovered", false) {
			orphan = candidate
			break
		}
	}
	if orphan == nil {
		t.Fatal("orphaned worktree was not recorded")
	}
	managed, err := managedWorktreePath(store, orphan)
	if err != nil {
		t.Fatalf("recorded orphan cannot be managed: %v", err)
	}
	resolved, err := canonical(path)
	if err != nil {
		t.Fatal(err)
	}
	if managed != resolved {
		t.Fatalf("managed orphan path = %q, want %q", managed, resolved)
	}
	orphan["worktree_path"] = filepath.Join(filepath.Dir(path), "different")
	if _, err := managedWorktreePath(store, orphan); err == nil {
		t.Fatal("orphan identity accepted a different path")
	}
}

func TestRecordOrphansPreservesExistingRegistry(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	path, err := canonical(stringValue(task, "worktree_path"))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := store.TaskPath(stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(registry); err != nil {
		t.Fatal(err)
	}
	orphanID := "orphan-" + sha256Hex([]byte(path))[:12]
	orphanRegistry, err := store.TaskPath(orphanID)
	if err != nil {
		t.Fatal(err)
	}
	preserved := []byte("preserve corrupt registry\n")
	if err := os.WriteFile(orphanRegistry, preserved, 0o600); err != nil {
		t.Fatal(err)
	}

	recordOrphans(store)
	current, err := os.ReadFile(orphanRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(preserved) {
		t.Fatalf("orphan discovery overwrote existing registry: %q", current)
	}
}

func TestWorktreePruneHonorsRepositoryActivity(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	activity, err := store.RepositoryActivityLock(repository, false, true)
	if err != nil {
		t.Fatal(err)
	}
	pruned, err := pruneRepositoryWorktrees(store, repository)
	if err != nil {
		t.Fatal(err)
	}
	if pruned {
		t.Fatal("worktree pruning ran while repository activity was leased")
	}
	if err := activity.Unlock(); err != nil {
		t.Fatal(err)
	}
	pruned, err = pruneRepositoryWorktrees(store, repository)
	if err != nil {
		t.Fatal(err)
	}
	if !pruned {
		t.Fatal("worktree pruning did not run after repository activity was released")
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

func TestTaskRebasesOntoRewrittenTarget(t *testing.T) {
	repository := testRepository(t)
	testCommitFile(t, repository, "old-base.txt", "old base\n", "feat: add old base")
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	result := testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "task\n", "feat: add task result")
	testCommand(t, repository, "git", "reset", "--hard", "-q", "HEAD^")
	target := testCommitFile(t, repository, "target.txt", "target\n", "feat: replace target history")

	current := finishTestTask(t, store, task, true)
	if status := stringValue(current, "status"); status != StatusIntegrated {
		t.Fatalf("status = %s, reason = %s", status, stringValue(current, "status_reason"))
	}
	if strategy := stringValue(current, "integration_strategy"); strategy != "rebase-diverged" {
		t.Fatalf("strategy = %s, want rebase-diverged", strategy)
	}
	integrated := stringValue(current, "integrated_commit")
	if integrated == result {
		t.Fatal("diverged task result was not replayed")
	}
	parents := strings.Fields(testCommand(t, repository, "git", "rev-list", "--parents", "-n", "1", integrated))
	if len(parents) != 2 || parents[1] != target {
		t.Fatalf("replayed commit parents = %v, want only %s", parents, target)
	}
	for _, name := range []string{"task.txt", "target.txt"} {
		if _, err := os.Stat(filepath.Join(repository, name)); err != nil {
			t.Fatalf("replayed target is missing %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(repository, "old-base.txt")); !os.IsNotExist(err) {
		t.Fatalf("replay restored unrelated abandoned base content: %v", err)
	}
}

func TestRewrittenTargetConflictPreservesResult(t *testing.T) {
	repository := testRepository(t)
	testCommitFile(t, repository, "old-base.txt", "old base\n", "feat: add old base")
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	result := testCommitFile(t, stringValue(task, "worktree_path"), "tracked.txt", "task\n", "fix: update task copy")
	testCommand(t, repository, "git", "reset", "--hard", "-q", "HEAD^")
	target := testCommitFile(t, repository, "tracked.txt", "target\n", "fix: update rewritten target")

	current := finishTestTask(t, store, task, true)
	if status := stringValue(current, "status"); status != StatusRecovery {
		t.Fatalf("status = %s, want %s", status, StatusRecovery)
	}
	if reason := stringValue(current, "status_reason"); !strings.Contains(reason, "integration conflict") {
		t.Fatalf("unexpected reason: %s", reason)
	}
	if head, _ := gitRef(repository, "refs/heads/main"); head != target {
		t.Fatalf("conflicting replay changed target to %s", head)
	}
	if stringValue(current, "result_commit") != result {
		t.Fatal("conflicting replay did not preserve the task result")
	}
}

func TestPublishRebasesActiveTaskOntoRewrittenTarget(t *testing.T) {
	repository := testRepository(t)
	testCommitFile(t, repository, "old-base.txt", "old base\n", "feat: add old base")
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	path := stringValue(task, "worktree_path")
	testCommitFile(t, path, "task.txt", "task\n", "feat: add task result")
	testCommand(t, repository, "git", "reset", "--hard", "-q", "HEAD^")
	target := testCommitFile(t, repository, "target.txt", "target\n", "feat: replace target history")
	task["status"] = StatusRunning
	task["process"] = processRecord(os.Getpid(), "agent", 0)
	if err := store.Save(task); err != nil {
		t.Fatal(err)
	}

	published, err := publishTaskCheckpoint(store, task)
	if err != nil {
		t.Fatal(err)
	}
	if strategy := stringValue(published, "strategy"); strategy != "rebase-diverged" {
		t.Fatalf("strategy = %s, want rebase-diverged", strategy)
	}
	publishedCommit := stringValue(published, "published_commit")
	if head, _ := gitRef(repository, "refs/heads/main"); head != publishedCommit {
		t.Fatalf("main = %s, want %s", head, publishedCommit)
	}
	if head, _ := gitRef(path, "HEAD"); head != publishedCommit {
		t.Fatalf("active task = %s, want %s", head, publishedCommit)
	}
	current, err := store.Load(stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(current, "base_sha") != target || stringValue(current, "result_commit") != publishedCommit {
		t.Fatalf("published task checkpoint was not rebased durably: %s", describe(current))
	}
	if diagnostic := recordMap(current, "last_publish_diagnostic"); stringValue(diagnostic, "outcome") != "published" || stringValue(diagnostic, "strategy") != "rebase-diverged" {
		t.Fatalf("successful publish diagnostic was not retained: %s", describe(diagnostic))
	}
}

func TestPublishFailureRetainsDiagnostic(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	testCommitFile(t, stringValue(task, "worktree_path"), "tracked.txt", "task\n", "fix: update task copy")
	testCommitFile(t, repository, "tracked.txt", "target\n", "fix: update target copy")
	task["status"] = StatusRunning
	task["process"] = processRecord(os.Getpid(), "agent", 0)
	if err := store.Save(task); err != nil {
		t.Fatal(err)
	}

	if _, err := publishTaskCheckpoint(store, task); err == nil {
		t.Fatal("conflicting publish unexpectedly succeeded")
	}
	current, err := store.Load(stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := recordMap(current, "last_publish_diagnostic")
	if stringValue(diagnostic, "outcome") != "failed" || !strings.Contains(stringValue(diagnostic, "reason"), "conflict") {
		t.Fatalf("failed publish diagnostic was not retained: %s", describe(diagnostic))
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

func TestTaskRefusesUnrelatedTargetHistory(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "task\n", "feat: add task result")
	tree := strings.TrimSpace(testCommand(t, repository, "git", "write-tree"))
	unrelated := strings.TrimSpace(testCommand(t, repository, "git", "commit-tree", tree, "-m", "chore: replace repository history"))
	testCommand(t, repository, "git", "reset", "--hard", "-q", unrelated)

	current := finishTestTask(t, store, task, true)
	if status := stringValue(current, "status"); status != StatusRecovery {
		t.Fatalf("status = %s, want %s", status, StatusRecovery)
	}
	if reason := stringValue(current, "status_reason"); !strings.Contains(reason, "unrelated history") {
		t.Fatalf("unexpected reason: %s", reason)
	}
	if head, _ := gitRef(repository, "refs/heads/main"); head != unrelated {
		t.Fatalf("unrelated target changed to %s", head)
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

func TestIntegrationStopsBeforeTargetMutationOnStateFailure(t *testing.T) {
	repository := testRepository(t)
	store := testStore(t)
	task := testTask(t, store, repository, createTaskOptions{})
	result := testCommitFile(t, stringValue(task, "worktree_path"), "task.txt", "task\n", "fix: add task result")
	targetBefore, err := gitRef(repository, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	task["result_commit"] = result
	task["status"] = StatusReady
	delete(task, "process")
	if err := store.Save(task); err != nil {
		t.Fatal(err)
	}
	path, _ := store.TaskPath(stringValue(task, "task_id"))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	success, err := integrateTask(store, task)
	if err == nil || success {
		t.Fatalf("integration continued without durable state: (%t, %v)", success, err)
	}
	if targetAfter, _ := gitRef(repository, "refs/heads/main"); targetAfter != targetBefore {
		t.Fatalf("target advanced despite state failure: %s -> %s", targetBefore, targetAfter)
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
