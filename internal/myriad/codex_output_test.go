package myriad

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCodexExitTailFilterPreservesTranscriptAndDropsSummary(t *testing.T) {
	transcript := "assistant transcript\r\nDisconnected from this task. quoted by the user\r\n"
	cleanup := codexTerminalCleanupMarker + "\x1b[?1004l\x1b[?25h"
	summary := "─ Worked for 11m 11s ─\r\n" +
		"Disconnected from this task. Any running work continues.\r\n" +
		"Reconnect: codex --remote unix:///tmp/control.sock resume thread-id\r\n" +
		"Stop the current turn: run codex --remote unix:///tmp/control.sock agents.\r\n" +
		"Token usage so far: total=123\r\n"

	var output bytes.Buffer
	filter := &codexExitTailFilter{destination: &output}
	input := []byte(transcript + cleanup + summary)
	for _, value := range input {
		if err := filter.write([]byte{value}); err != nil {
			t.Fatal(err)
		}
	}
	if err := filter.finish(); err != nil {
		t.Fatal(err)
	}
	want := transcript + cleanup + codexTerminalSafeReset
	if got := output.String(); got != want {
		t.Fatalf("filtered output = %q, want %q", got, want)
	}
}

func TestCodexExitTailFilterDropsDisconnectWithoutUsage(t *testing.T) {
	input := "conversation\r\n" + codexTerminalCleanupMarker + "\x1b[?25h" +
		"Disconnected from this task. The current turn was stopped.\r\n"
	var output bytes.Buffer
	filter := &codexExitTailFilter{destination: &output}
	if err := filter.write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	if err := filter.finish(); err != nil {
		t.Fatal(err)
	}
	want := "conversation\r\n" + codexTerminalCleanupMarker + "\x1b[?25h" + codexTerminalSafeReset
	if got := output.String(); got != want {
		t.Fatalf("filtered output = %q, want %q", got, want)
	}
}

func TestCodexExitTailFilterFailsOpenWithoutStandardSummary(t *testing.T) {
	for _, input := range []string{
		"conversation\r\n" + codexTerminalCleanupMarker + "fatal diagnostic\r\n",
		"conversation\r\n" + codexTerminalCleanupMarker + "paused" + codexTerminalResumeMarker + "continued\r\n",
	} {
		var output bytes.Buffer
		filter := &codexExitTailFilter{destination: &output}
		if err := filter.write([]byte(input)); err != nil {
			t.Fatal(err)
		}
		if err := filter.finish(); err != nil {
			t.Fatal(err)
		}
		if got := output.String(); got != input {
			t.Fatalf("filtered output = %q, want unchanged %q", got, input)
		}
	}
}

func TestCodexExitTailFilterFailsOpenWhenTailIsUnexpectedlyLarge(t *testing.T) {
	input := codexTerminalCleanupMarker + strings.Repeat("x", codexExitTailLimit+1)
	var output bytes.Buffer
	filter := &codexExitTailFilter{destination: &output}
	if err := filter.write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	if err := filter.finish(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != input {
		t.Fatalf("large tail changed: got %d bytes, want %d", len(got), len(input))
	}
}

func TestCodexOutputRelayEligibility(t *testing.T) {
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
			if got := codexOutputRelayEligible(test.command, test.term, test.inputTTY, test.outTTY); got != test.want {
				t.Fatalf("eligibility = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCodexOutputPTYIsTTYAndFiltersExitTail(t *testing.T) {
	master, slave, err := openCodexOutputPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = master.Close() }()
	defer func() { _ = slave.Close() }()

	var output bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- relayCodexOutput(master, &output) }()
	command := exec.Command("sh", "-c", "test -t 1 || exit 42; printf 'pty transcript\\n\\033[?2004l\\033[?25hDisconnected from this task. Any running work continues.\\n'")
	command.Stdout = slave
	command.Stderr = slave
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := slave.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "pty transcript\n") {
		t.Fatalf("transcript missing from relayed output: %q", got)
	}
	if strings.Contains(got, "Disconnected from this task") {
		t.Fatalf("disconnect summary survived relayed output: %q", got)
	}
}

func TestCodexOutputRelayCopiesTerminalSize(t *testing.T) {
	sourceMaster, sourceSlave, err := openCodexOutputPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sourceMaster.Close() }()
	defer func() { _ = sourceSlave.Close() }()
	targetMaster, targetSlave, err := openCodexOutputPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = targetMaster.Close() }()
	defer func() { _ = targetSlave.Close() }()

	want := &unix.Winsize{Row: 47, Col: 123, Xpixel: 456, Ypixel: 789}
	if err := unix.IoctlSetWinsize(int(sourceMaster.Fd()), unix.TIOCSWINSZ, want); err != nil {
		t.Fatal(err)
	}
	relay := &codexOutputRelay{master: targetMaster}
	if err := relay.resize(sourceMaster); err != nil {
		t.Fatal(err)
	}
	got, err := unix.IoctlGetWinsize(int(targetMaster.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *want {
		t.Fatalf("target size = %#v, want %#v", got, want)
	}
}
