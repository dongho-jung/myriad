package myriad

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
	server, err := spawnCodexServer([]string{"codex", "app-server", "--listen", "unix://" + socket}, os.Environ())
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
		"/state/hooks/hash/myriad __provision-hook",
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
		"/tmp/control.sock", []string{"/project"},
	)
	joined := strings.Join(command, "\n")
	if strings.Contains(joined, "tui.show_tooltips=true") {
		t.Fatalf("stale tooltip override survived: %#v", command)
	}
	if count := strings.Count(joined, "tui.status_line="); count != 1 {
		t.Fatalf("remote command has %d status line overrides: %#v", count, command)
	}
	for _, expected := range []string{"--remote", "unix:///tmp/control.sock", "tui.show_tooltips=false", codexManagedStatusLine} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("remote command is missing %q: %#v", expected, command)
		}
	}
	if !strings.HasPrefix(codexManagedStatusLine, `tui.status_line=["thread-title","pull-request-number",`) {
		t.Fatalf("managed status line does not place Codex's linked PR item after its context title: %s", codexManagedStatusLine)
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

func TestCodexLauncherRoutesGlobalOptions(t *testing.T) {
	tests := []struct {
		arguments []string
		want      launcherRoute
	}{
		{[]string{"-m", "gpt-5.6-sol", "review", "--uncommitted"}, routeCurrent},
		{[]string{"-m", "gpt-5.6-sol", "resume", "--last"}, routeManagedFresh},
		{[]string{"-m", "gpt-5.6-sol", "exec", "go test ./..."}, routeManagedFresh},
		{[]string{"-c", "model=\"gpt-5.6-sol\"", "agents"}, routeDirect},
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
