package myriad

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sys/unix"
)

func testCodexRecoveryServer(t *testing.T, socketPath, cwd string, missingRollout bool) <-chan Record {
	t.Helper()
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	listParams := make(chan Record, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, upgradeErr := upgrader.Upgrade(writer, request, nil)
		if upgradeErr != nil {
			return
		}
		defer func() { _ = connection.Close() }()
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
			case "thread/list":
				listParams <- anyRecord(message["params"])
				_ = connection.WriteJSON(Record{"id": requestID, "result": Record{
					"data": []any{Record{"id": "thread-one", "cwd": cwd}},
				}})
			case "thread/resume":
				if missingRollout {
					_ = connection.WriteJSON(Record{"id": requestID, "error": Record{
						"code": -32600, "message": "no rollout found for thread id thread-one",
					}})
				} else {
					_ = connection.WriteJSON(Record{"id": requestID, "result": Record{"reasoningEffort": "high"}})
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
	return listParams
}

func TestDirectCLIReportsExecutableStartFailure(t *testing.T) {
	code, err := directCLI([]string{filepath.Join(t.TempDir(), "missing")}, t.TempDir(), os.Environ())
	if code != 127 || err == nil || !strings.Contains(err.Error(), "cannot execute") {
		t.Fatalf("directCLI() = (%d, %v), want a reported start failure", code, err)
	}
}

func TestCodexAcceptsProvisionHookConfig(t *testing.T) {
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex is not installed")
	}
	command := exec.Command(codex,
		"-c", codexProvisionHookConfig("/tmp/myriad"),
		"--dangerously-bypass-hook-trust", "features", "list",
	)
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Codex rejected Myriad hook config: %v\n%s", err, output)
	}
}

func TestCodexAppServerUnixTransport(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex is not installed")
	}
	socket := filepath.Join(t.TempDir(), "app-server.sock")
	serverCommand, err := codexAppServerCommand([]string{"codex"}, socket, true, "/tmp/myriad-protocol-test")
	if err != nil {
		t.Fatal(err)
	}
	server, err := spawnCodexServer(serverCommand, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	defer stopCodexServer(server, true)
	if err := waitForCodexServer(server, socket); err != nil {
		t.Fatal(err)
	}
	rpc, err := dialCodex(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.close()
	if err := rpc.initialize("Myriad protocol test"); err != nil {
		t.Fatal(err)
	}
	result, err := rpc.request(2, "model/list", Record{"limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(anySlice(anyRecord(result)["data"])) == 0 {
		t.Fatal("Codex App Server returned no models")
	}
}

func TestWaitForCodexServerReturnsAfterEarlyExit(t *testing.T) {
	server, err := spawnCodexServer([]string{"sh", "-c", "exit 7"}, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = waitForCodexServer(server, filepath.Join(t.TempDir(), "missing.sock"))
	if err == nil || !strings.Contains(err.Error(), "exited before opening") {
		t.Fatalf("unexpected App Server error: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("early App Server exit took %s to detect", elapsed)
	}
}

func TestStopCodexServerReapsPromptly(t *testing.T) {
	server, err := spawnCodexServer([]string{"sleep", "30"}, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	stopCodexServer(server, true)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("App Server shutdown took %s", elapsed)
	}
	if !server.reaped {
		t.Fatal("App Server child was not reaped")
	}
}

func TestCodexServerStopsWhenParentDies(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "server.pid")
	command := exec.Command(os.Args[0], "-test.run=^$")
	command.Env = append(os.Environ(), "MYRIAD_TEST_CODEX_SERVER_PID_PATH="+pidPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("App Server parent failed: %v\n%s", err, output)
	}
	waitForFile(t, pidPath)
	payload, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Kill(-pid, unix.SIGKILL) }()
	waitForProcessExit(t, pid)
}

func TestFreshManagedCodexAppServerDoesNotResumeEmptyThread(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex is not installed")
	}
	t.Setenv("MYRIAD_HARNESS", "myriad")
	t.Setenv("MYRIAD_TASK_ID", "fresh-codex-test")
	t.Setenv("MYRIAD_BRANCH", "")
	store := testStore(t)
	socketPath := filepath.Join(t.TempDir(), "codex.sock")
	server, command, err := startCodexAppServer(
		store,
		[]string{"codex", "--dangerously-bypass-approvals-and-sandbox"},
		socketPath,
		[]string{t.TempDir()},
		t.TempDir(),
		os.Environ(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if server == nil {
		t.Fatal("fresh Codex launch did not start its App Server")
	}
	defer stopCodexServer(server, true)
	if codexSubcommand(command) != "" {
		t.Fatalf("fresh managed launch unexpectedly resumes an empty thread: %#v", command)
	}
	if !strings.Contains(strings.Join(command, "\n"), codexManagedStatusLine) {
		t.Fatalf("fresh managed launch is missing its thread title status item: %#v", command)
	}
}

func TestCodexProvisionServerUsesPinnedHookBypass(t *testing.T) {
	command, err := codexAppServerCommand(
		[]string{"codex", "--dangerously-bypass-approvals-and-sandbox"},
		"/tmp/myriad-test.sock", true, "/state/hooks/hash/myriad",
	)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(command, "\n")
	for _, expected := range []string{
		"--dangerously-bypass-hook-trust",
		"hooks.UserPromptSubmit=",
		hookCommand(internalProvision, "/state/hooks/hash/myriad"),
		"unix:///tmp/myriad-test.sock",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("server command is missing %q: %#v", expected, command)
		}
	}
	if strings.Contains(joined, "trusted_hash") || strings.Contains(joined, "hooks.state") {
		t.Fatalf("server command retained brittle hook trust state: %#v", command)
	}
}

func TestCodexProvisionHookCommandPreservesShellArguments(t *testing.T) {
	launcher := filepath.Join(t.TempDir(), "myriad-$HOME-'quoted'")
	command := exec.Command("sh", "-c", "set -- "+hookCommand(internalProvision, launcher)+`; printf '%s\n%s\n' "$1" "$2"`)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := launcher + "\n" + internalProvision + "\n"
	if string(output) != want {
		t.Fatalf("hook argv = %q, want %q", output, want)
	}
}

func TestCodexHookRuntimeRejectsSymlink(t *testing.T) {
	store := testStore(t)
	executable, err := executablePath()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(store.HookRuntimes, sha256Hex(payload))
	if err := ensurePrivateDirectory(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(directory, "myriad")); err != nil {
		t.Fatal(err)
	}
	if _, err := materializeCodexHookRuntime(store); err == nil {
		t.Fatal("symlink hook runtime was trusted")
	}
}

func TestCodexHookRuntimeRestoresExecutableMode(t *testing.T) {
	store := testStore(t)
	path, err := materializeCodexHookRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	path, err = materializeCodexHookRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("hook runtime mode = %04o, want 0700", info.Mode().Perm())
	}
}

func TestCodexTUICommandOmitsUnmanagedThreadTitle(t *testing.T) {
	command := codexTUICommand(nil, "/project")
	joined := strings.Join(command, "\n")
	if strings.Contains(joined, "thread-title") {
		t.Fatalf("unmanaged command exposes a thread title: %#v", command)
	}
	if !strings.Contains(joined, codexDirectStatusLine) {
		t.Fatalf("unmanaged command is missing its status line: %#v", command)
	}
	if !strings.Contains(codexDirectStatusLine, `"pull-request-number"`) {
		t.Fatalf("unmanaged status line does not expose Codex's linked PR item: %s", codexDirectStatusLine)
	}
}

func TestCodexRemoteCommandKeepsLatestTUISettings(t *testing.T) {
	command := codexRemoteCommand(
		[]string{
			"codex",
			"-c", "tui.show_tooltips=true",
			"-c", codexDirectStatusLine,
			"--dangerously-bypass-approvals-and-sandbox",
		},
		"/tmp/control.sock", []string{"/project"}, codexManagedStatusLine, "/project",
	)
	joined := strings.Join(command, "\n")
	if strings.Contains(joined, "tui.show_tooltips=true") {
		t.Fatalf("stale tooltip override survived: %#v", command)
	}
	if count := strings.Count(joined, "tui.status_line="); count != 1 {
		t.Fatalf("remote command has %d status line overrides: %#v", count, command)
	}
	for _, expected := range []string{"--remote", "unix:///tmp/control.sock", "--cd", "/project", "tui.show_tooltips=false", codexManagedStatusLine} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("remote command is missing %q: %#v", expected, command)
		}
	}
	if !strings.HasPrefix(codexManagedStatusLine, `tui.status_line=["thread-title","pull-request-number",`) {
		t.Fatalf("managed status line does not place Codex's linked PR item after its context title: %s", codexManagedStatusLine)
	}
}

func TestCodexRemoteCommandKeepsExplicitWorkingDirectory(t *testing.T) {
	command := codexRemoteCommand(
		[]string{"codex", "--cd", "/selected"},
		"/tmp/control.sock", []string{"/project"}, codexManagedStatusLine, "/project",
	)
	count := 0
	for _, argument := range command {
		if argument == "--cd" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("remote command has %d working directory flags: %#v", count, command)
	}
	if !slices.Contains(command, "/selected") {
		t.Fatalf("remote command lost its explicit working directory: %#v", command)
	}
}

func TestFreshManagedCodexStartsWithThreadTitle(t *testing.T) {
	fresh := []string{"codex", "--dangerously-bypass-approvals-and-sandbox"}
	command := codexRemoteCommand(fresh, "/tmp/control.sock", []string{"/project"}, codexManagedStatusLine, "/project")
	if !strings.Contains(strings.Join(command, "\n"), codexManagedStatusLine) {
		t.Fatalf("fresh managed command is missing its thread title status item: %#v", command)
	}
	if codexSubcommand(command) != "" {
		t.Fatalf("fresh managed command unexpectedly resumes a thread: %#v", command)
	}
}

func TestCodexRecoveryFallsBackFromMissingRollout(t *testing.T) {
	cwd := t.TempDir()
	socketPath := filepath.Join(t.TempDir(), "codex.sock")
	listParams := testCodexRecoveryServer(t, socketPath, cwd, true)
	threadID, effort, err := resumableCodexThread(socketPath, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if threadID != "" || effort != "" {
		t.Fatalf("unusable thread = (%q, %q), want a fresh-chat fallback", threadID, effort)
	}
	if params := <-listParams; params["useStateDbOnly"] != nil {
		t.Fatalf("recovery skipped Codex scan-and-repair: %#v", params)
	}
}

func TestCodexRecoveryKeepsStoredReasoningEffort(t *testing.T) {
	cwd := t.TempDir()
	socketPath := filepath.Join(t.TempDir(), "codex.sock")
	testCodexRecoveryServer(t, socketPath, cwd, false)
	threadID, effort, err := resumableCodexThread(socketPath, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if threadID != "thread-one" || effort != "high" {
		t.Fatalf("resumable thread = (%q, %q), want (thread-one, high)", threadID, effort)
	}
}

func TestCodexWorkingDirectoryIsNormalized(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := ensurePrivateDirectory(project); err != nil {
		t.Fatal(err)
	}
	command, selected, err := normalizeCodexWorkingDirectory([]string{"codex", "--cd", "project"}, root)
	if err != nil {
		t.Fatal(err)
	}
	if selected != project || !reflect.DeepEqual(command, []string{"codex", "--cd", project}) {
		t.Fatalf("command = %#v, selected = %q", command, selected)
	}
}

func TestCodexReasoningEffortInsertedBeforeSubcommand(t *testing.T) {
	command, err := codexWithReasoningEffort([]string{"codex", "resume", "thread-id"}, "ultra")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "-c", `model_reasoning_effort="ultra"`, "resume", "thread-id"}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("command = %#v, want %#v", command, want)
	}
}

func TestCodexSubcommandsMatchCurrentCLI(t *testing.T) {
	for _, subcommand := range []string{"agents", "queue", "resume", "review", "app-server"} {
		if got := codexSubcommand([]string{"codex", subcommand}); got != subcommand {
			t.Fatalf("subcommand %q parsed as %q", subcommand, got)
		}
	}
}

func TestCodexRoutingContractMatchesInstalledCLI(t *testing.T) {
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex is not installed")
	}
	output, err := exec.Command(codex, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("cannot inspect installed Codex CLI: %v\n%s", err, output)
	}
	lines := strings.Split(string(output), "\n")
	inCommands := false
	commandsFound := 0
	commandLine := regexp.MustCompile(`^  ([a-z][a-z0-9-]*)\s{2,}`)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "Commands:" {
			inCommands = true
			continue
		}
		if !inCommands {
			continue
		}
		if trimmed == "" {
			break
		}
		match := commandLine.FindStringSubmatch(line)
		if len(match) > 0 {
			commandsFound++
			if !codexSubcommands[match[1]] {
				t.Fatalf("installed Codex command %q has no explicit Myriad routing rule", match[1])
			}
		}
	}
	if commandsFound == 0 {
		t.Fatal("could not parse any commands from installed Codex help")
	}

	valueOption := regexp.MustCompile(`^\s+(?:(-[A-Za-z]), )?(--[a-z0-9-]+) <[^>]+>(?:\.\.\.)?\s*$`)
	valueOptionsFound := 0
	for _, line := range lines {
		match := valueOption.FindStringSubmatch(line)
		if len(match) == 0 {
			continue
		}
		valueOptionsFound++
		for _, option := range match[1:3] {
			if option != "" && !codexGlobalValueOptions[option] {
				t.Fatalf("installed Codex value option %q is missing from Myriad's argument parser", option)
			}
		}
	}
	if valueOptionsFound == 0 {
		t.Fatal("could not parse any value options from installed Codex help")
	}
}

func TestCodexLauncherRoutesGlobalOptions(t *testing.T) {
	tests := []struct {
		arguments []string
		want      launcherRoute
	}{
		{[]string{"-m", "gpt-5.6-sol", "review", "--uncommitted"}, routeCurrent},
		{[]string{"-m", "gpt-5.6-sol", "resume", "--last"}, routeManagedFresh},
		{[]string{"-m", "gpt-5.6-sol", "exec", "go test ./..."}, routeManagedFresh},
		{[]string{"-c", "model=\"gpt-5.6-sol\"", "agents"}, routeDirect},
		{[]string{"--remote", "unix:///tmp/codex.sock"}, routeDirect},
		{[]string{"queue", "thread-id", "keep going"}, routeDirect},
		{[]string{"fix the parser"}, routeDescription},
		{[]string{"--search"}, routeManaged},
	}
	for _, test := range tests {
		command := codexTUICommand(test.arguments, "/project")
		if got := codexLauncherRoute(test.arguments, command); got != test.want {
			t.Errorf("arguments %#v route to %s, want %s", test.arguments, got, test.want)
		}
	}
}

func TestManagedTaskRejectsExternalCodexAppServer(t *testing.T) {
	command := []string{"codex", "--remote", "unix:///tmp/codex.sock"}
	if err := validateForegroundAgentCommand("codex", command, true); err == nil {
		t.Fatal("managed task accepted an external Codex App Server")
	}
	if codexUsesRemoteAppServer([]string{"codex", "--", "--remote"}) {
		t.Fatal("prompt text after -- was mistaken for a remote App Server option")
	}
	wrapped := []string{"env", "-u", "CODEX_TOKEN", "codex", "--remote", "unix:///tmp/codex.sock"}
	if err := validateForegroundAgentCommand("codex", wrapped, true); err == nil {
		t.Fatal("managed task accepted an env-wrapped external Codex App Server")
	}
	for _, command := range [][]string{
		{"env", "-uCODEX_TOKEN", "codex", "--remote", "unix:///tmp/codex.sock"},
		{"env", "-a", "my-codex", "codex", "--remote", "unix:///tmp/codex.sock"},
		{"env", "--block-signal=TERM", "codex", "--remote", "unix:///tmp/codex.sock"},
	} {
		if err := validateForegroundAgentCommand("codex", command, true); err == nil {
			t.Errorf("managed task accepted env-wrapped remote command %#v", command)
		}
	}
	if err := validateForegroundAgentCommand("codex", []string{"env", "-S", "codex --remote unix:///tmp/codex.sock"}, true); err == nil {
		t.Fatal("managed task accepted an opaque env split-string command")
	}
}

func TestCommandExecutableIndexParsesEnvOptions(t *testing.T) {
	commands := [][]string{
		{"env", "-uCODEX_TOKEN", "codex"},
		{"env", "-a", "my-codex", "codex"},
		{"env", "--argv0=my-codex", "codex"},
		{"env", "--block-signal=TERM", "codex"},
		{"env", "-", "CODEX_TOKEN=value", "--", "codex"},
	}
	for _, command := range commands {
		if index := commandExecutableIndex(command, "codex"); index != len(command)-1 {
			t.Errorf("executable index for %#v = %d, want %d", command, index, len(command)-1)
		}
	}
	for _, command := range [][]string{
		{"env", "-S", "codex --remote unix:///tmp/server.sock"},
		{"env", "--split-string=codex --remote unix:///tmp/server.sock"},
	} {
		if index := commandExecutableIndex(command, "codex"); index >= 0 {
			t.Errorf("opaque command %#v exposed executable at %d", command, index)
		}
	}
}

func TestManagedCodexArgumentsStopAtPromptBoundary(t *testing.T) {
	worktree := t.TempDir()
	command := []string{"codex", "--", "--add-dir", worktree}
	got := managedAgentCommand(Record{"worktree_path": worktree}, command)
	want := []string{"codex", "--add-dir", worktree, "--", "--add-dir", worktree}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("managed command = %#v, want %#v", got, want)
	}

	config := "tui.status_line=prompt-text"
	got = stripManagedCodexTUIConfigs([]string{"codex", "-c", "tui.status_line=old", "--", "-c", config}, 0)
	want = []string{"codex", "--", "-c", config}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stripped command = %#v, want %#v", got, want)
	}
}

func TestClaudeSessionManagementRunsDirectly(t *testing.T) {
	for _, subcommand := range []string{"agents", "attach", "logs", "respawn", "rm", "stop", "kill"} {
		if !claudeDirectInvocation([]string{subcommand, "session-id"}) {
			t.Errorf("Claude %s was not routed directly", subcommand)
		}
		command := []string{"claude", subcommand, "session-id"}
		if err := validateForegroundAgentCommand("claude", command, true); err == nil {
			t.Errorf("managed task accepted Claude %s", subcommand)
		}
	}
	if claudeDirectInvocation([]string{"ultrareview"}) {
		t.Fatal("Claude ultrareview lost its exact-checkout routing")
	}
}
