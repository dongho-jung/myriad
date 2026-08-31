package myriad

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	if pidPath := os.Getenv("MYRIAD_TEST_CODEX_SERVER_PID_PATH"); pidPath != "" {
		server, err := spawnCodexServer([]string{"sleep", "30"}, os.Environ())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", server.Command.Process.Pid)), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == internalValidate {
		code, err := validationSupervisor(os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "myriad:", err)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func testRepository(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	testCommand(t, root, "git", "init", "-q", "-b", "main")
	testCommand(t, root, "git", "config", "user.name", "Myriad Test")
	testCommand(t, root, "git", "config", "user.email", "myriad@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testCommand(t, root, "git", "add", "tracked.txt")
	testCommand(t, root, "git", "commit", "-q", "-m", "chore: initial state")
	if err := os.WriteFile(filepath.Join(root, ".git", "info", "exclude"), []byte(MemoryName+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func testStore(t *testing.T) *Store {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("MYRIAD_STATE_DIR", state)
	store, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testCommand(t *testing.T, cwd string, argv ...string) string {
	t.Helper()
	result, err := checkedCommand(cwd, argv...)
	if err != nil {
		t.Fatalf("%s: %v", displayCommand(argv), err)
	}
	return result.Stdout
}

func testCommitFile(t *testing.T, worktree, path, contents, message string) string {
	t.Helper()
	full := filepath.Join(worktree, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	testCommand(t, worktree, "git", "add", path)
	testCommand(t, worktree, "git", "commit", "-q", "-m", message)
	head, err := gitRef(worktree, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func testTask(t *testing.T, store *Store, repository string, options createTaskOptions) Record {
	t.Helper()
	if options.LaunchCWD == "" {
		options.LaunchCWD = repository
	}
	if options.Agent == "" {
		options.Agent = "custom"
	}
	if options.Description == "" {
		options.Description = "fix test lifecycle"
	}
	task, err := createTask(store, options)
	if err != nil {
		t.Fatal(err)
	}
	if !taskWorktreeReady(task) {
		t.Fatal("test task was not provisioned")
	}
	return task
}

func finishTestTask(t *testing.T, store *Store, task Record, integrate bool) Record {
	t.Helper()
	recordAgentExit(task, 0, false)
	delete(task, "process")
	if err := store.Save(task); err != nil {
		t.Fatal(err)
	}
	if err := finalizeTask(store, task, integrate, false); err != nil {
		t.Fatal(err)
	}
	current, err := store.Load(stringValue(task, "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	return current
}
