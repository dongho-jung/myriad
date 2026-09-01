package myriad

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestPrivateTerminalScreenRestoresOnce(t *testing.T) {
	var output strings.Builder
	screen := &privateTerminalScreen{output: &output}
	if err := screen.enter(); err != nil {
		t.Fatal(err)
	}
	if err := screen.enter(); err != nil {
		t.Fatal(err)
	}
	if err := screen.restore(); err != nil {
		t.Fatal(err)
	}
	if err := screen.restore(); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), enterAlternateScreen+leaveAlternateScreen; got != want {
		t.Fatalf("terminal sequence = %q, want %q", got, want)
	}
}

func TestPrivateCodexScreenEligibility(t *testing.T) {
	tests := []struct {
		name     string
		command  []string
		term     string
		inputTTY bool
		outTTY   bool
		want     bool
	}{
		{name: "interactive Codex", command: []string{"codex"}, term: "xterm-kitty", inputTTY: true, outTTY: true, want: true},
		{name: "resume", command: []string{"codex", "resume", "thread-id"}, term: "xterm-kitty", inputTTY: true, outTTY: true, want: true},
		{name: "noninteractive Codex", command: []string{"codex", "exec", "task"}, term: "xterm-kitty", inputTTY: true, outTTY: true},
		{name: "Claude", command: []string{"claude"}, term: "xterm-kitty", inputTTY: true, outTTY: true},
		{name: "dumb terminal", command: []string{"codex"}, term: "dumb", inputTTY: true, outTTY: true},
		{name: "redirected output", command: []string{"codex"}, term: "xterm-kitty", inputTTY: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := privateCodexScreenEligible(test.command, test.term, test.inputTTY, test.outTTY); got != test.want {
				t.Fatalf("eligibility = %v, want %v", got, test.want)
			}
		})
	}
}

func TestPrivateTerminalScreenKeepsActiveOnRestoreFailure(t *testing.T) {
	screen := &privateTerminalScreen{output: failingWriter{}, active: true}
	if err := screen.restore(); err == nil {
		t.Fatal("restore unexpectedly succeeded")
	}
	if !screen.active {
		t.Fatal("failed restore disarmed the terminal screen")
	}
}

func TestSupervisorExitReportsWhetherCleanupRan(t *testing.T) {
	normalExit := exec.Command("sh", "-c", "exit 7").Run()
	if !supervisorExitedNormally(normalExit) {
		t.Fatal("ordinary nonzero exit was treated as an unclean supervisor death")
	}
	signaledExit := exec.Command("sh", "-c", "kill -KILL $$").Run()
	if supervisorExitedNormally(signaledExit) {
		t.Fatal("signal death was treated as an ordinary supervisor exit")
	}
	if supervisorExitedNormally(errors.New("wait unavailable")) {
		t.Fatal("unknown wait failure was treated as an ordinary supervisor exit")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}
