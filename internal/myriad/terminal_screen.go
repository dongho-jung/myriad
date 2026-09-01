package myriad

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	enterAlternateScreen = "\x1b[?1049h"
	leaveAlternateScreen = "\x1b[?1049l"
)

type privateTerminalScreen struct {
	output io.Writer
	active bool
}

func startPrivateCodexScreen(command []string, input, output *os.File) (*privateTerminalScreen, error) {
	if !privateCodexScreenEligible(
		command,
		os.Getenv("TERM"),
		terminalFile(input),
		terminalFile(output),
	) {
		return nil, nil
	}
	screen := &privateTerminalScreen{output: output}
	if err := screen.enter(); err != nil {
		return nil, fail("cannot enter the managed Codex terminal screen: %v", err)
	}
	return screen, nil
}

func privateCodexScreenEligible(command []string, term string, inputTerminal, outputTerminal bool) bool {
	return interactiveCodexCommand(command) && term != "" && term != "dumb" && inputTerminal && outputTerminal
}

func terminalFile(file *os.File) bool {
	if file == nil {
		return false
	}
	_, err := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS)
	return err == nil
}

func (screen *privateTerminalScreen) enter() error {
	if screen == nil || screen.active {
		return nil
	}
	if _, err := io.WriteString(screen.output, enterAlternateScreen); err != nil {
		return err
	}
	screen.active = true
	return nil
}

func (screen *privateTerminalScreen) restore() error {
	if screen == nil || !screen.active {
		return nil
	}
	if _, err := io.WriteString(screen.output, leaveAlternateScreen); err != nil {
		return err
	}
	screen.active = false
	return nil
}

func (screen *privateTerminalScreen) disarm() {
	if screen != nil {
		screen.active = false
	}
}

func supervisorExitedNormally(waitErr error) bool {
	if waitErr == nil {
		return true
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Exited()
}
