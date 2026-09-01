package myriad

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	codexTerminalCleanupMarker = "\x1b[?2004l"
	codexTerminalResumeMarker  = "\x1b[?2004h"
	codexExitTailLimit         = 1 << 20
	codexTerminalSafeReset     = "\x1b[0m\x1b[?25h"
)

type codexOutputRelay struct {
	master      *os.File
	slave       *os.File
	destination io.Writer
	done        chan error
}

// prepareCodexOutputRelay leaves stdin attached to the real foreground
// terminal and gives Codex only a private output PTY. Codex therefore keeps its
// normal interactive input and scrollback behavior while Myriad can inspect the
// final output tail before forwarding it.
func prepareCodexOutputRelay(command []string, input, output *os.File) (*codexOutputRelay, error) {
	if !codexOutputRelayEligible(command, os.Getenv("TERM"), terminalFile(input), terminalFile(output)) {
		return nil, nil
	}
	master, slave, err := openCodexOutputPTY()
	if err != nil {
		return nil, fail("cannot prepare Codex terminal output relay: %v", err)
	}
	relay := &codexOutputRelay{master: master, slave: slave, destination: output}
	if err := relay.resize(output); err != nil {
		relay.abort()
		return nil, fail("cannot size Codex terminal output relay: %v", err)
	}
	return relay, nil
}

func codexOutputRelayEligible(command []string, term string, inputTerminal, outputTerminal bool) bool {
	return interactiveCodexCommand(command) && term != "" && term != "dumb" && inputTerminal && outputTerminal
}

func terminalFile(file *os.File) bool {
	if file == nil {
		return false
	}
	_, err := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS)
	return err == nil
}

func openCodexOutputPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			_ = master.Close()
		}
	}()
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		return nil, nil, err
	}
	if number < 0 {
		return nil, nil, fmt.Errorf("invalid pseudoterminal number %d", number)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		return nil, nil, err
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0) //nolint:gosec // The kernel supplied the numeric PTY index.
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			_ = slave.Close()
		}
	}()
	termios, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		return nil, nil, err
	}
	termios.Oflag &^= unix.OPOST
	if err := unix.IoctlSetTermios(int(slave.Fd()), unix.TCSETS, termios); err != nil {
		return nil, nil, err
	}
	return master, slave, nil
}

func (relay *codexOutputRelay) attach(command *exec.Cmd) {
	command.Stdout = relay.slave
	command.Stderr = relay.slave
}

func (relay *codexOutputRelay) started() {
	_ = relay.slave.Close()
	relay.slave = nil
	relay.done = make(chan error, 1)
	go func() {
		relay.done <- relayCodexOutput(relay.master, relay.destination)
	}()
}

func (relay *codexOutputRelay) resize(source *os.File) error {
	if relay == nil || relay.master == nil || source == nil {
		return nil
	}
	size, err := unix.IoctlGetWinsize(int(source.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return err
	}
	return unix.IoctlSetWinsize(int(relay.master.Fd()), unix.TIOCSWINSZ, size)
}

func (relay *codexOutputRelay) wait() error {
	if relay == nil || relay.done == nil {
		return nil
	}
	err := <-relay.done
	relay.done = nil
	if relay.master != nil {
		_ = relay.master.Close()
		relay.master = nil
	}
	return err
}

func (relay *codexOutputRelay) abort() {
	if relay == nil {
		return
	}
	if relay.slave != nil {
		_ = relay.slave.Close()
		relay.slave = nil
	}
	if relay.master != nil {
		_ = relay.master.Close()
		relay.master = nil
	}
}

func relayCodexOutput(source *os.File, destination io.Writer) error {
	filter := &codexExitTailFilter{destination: destination}
	buffer := make([]byte, 32*1024)
	var readErr error
	for {
		count, err := source.Read(buffer)
		if count > 0 {
			if writeErr := filter.write(buffer[:count]); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, unix.EIO) && !errors.Is(err, os.ErrClosed) {
				readErr = err
			}
			break
		}
	}
	return errors.Join(readErr, filter.finish())
}

type codexExitTailFilter struct {
	destination io.Writer
	probe       []byte
	tail        []byte
}

func (filter *codexExitTailFilter) write(input []byte) error {
	if len(filter.tail) > 0 {
		return filter.writeTail(input)
	}
	filter.probe = append(filter.probe, input...)
	marker := []byte(codexTerminalCleanupMarker)
	if index := bytes.Index(filter.probe, marker); index >= 0 {
		if err := writeAll(filter.destination, filter.probe[:index]); err != nil {
			return err
		}
		filter.tail = append(filter.tail, filter.probe[index:]...)
		filter.probe = nil
		return filter.checkTail()
	}
	keep := len(marker) - 1
	if len(filter.probe) <= keep {
		return nil
	}
	flush := len(filter.probe) - keep
	if err := writeAll(filter.destination, filter.probe[:flush]); err != nil {
		return err
	}
	filter.probe = append(filter.probe[:0], filter.probe[flush:]...)
	return nil
}

func (filter *codexExitTailFilter) writeTail(input []byte) error {
	filter.tail = append(filter.tail, input...)
	return filter.checkTail()
}

func (filter *codexExitTailFilter) checkTail() error {
	resume := []byte(codexTerminalResumeMarker)
	searchFrom := len(codexTerminalCleanupMarker)
	if len(filter.tail) > searchFrom {
		if relative := bytes.Index(filter.tail[searchFrom:], resume); relative >= 0 {
			end := searchFrom + relative + len(resume)
			if err := writeAll(filter.destination, filter.tail[:end]); err != nil {
				return err
			}
			remainder := append([]byte{}, filter.tail[end:]...)
			filter.tail = nil
			return filter.write(remainder)
		}
	}
	if len(filter.tail) <= codexExitTailLimit {
		return nil
	}
	// Contract drift must not hide arbitrary output. If Codex does not finish a
	// normal terminal cleanup within this bounded tail, emit everything.
	pending := filter.tail
	filter.tail = nil
	return writeAll(filter.destination, pending)
}

func (filter *codexExitTailFilter) finish() error {
	if len(filter.tail) == 0 {
		err := writeAll(filter.destination, filter.probe)
		filter.probe = nil
		return err
	}
	output, suppressed := suppressCodexExitSummary(filter.tail)
	if err := writeAll(filter.destination, output); err != nil {
		return err
	}
	if suppressed {
		if err := writeAll(filter.destination, []byte(codexTerminalSafeReset)); err != nil {
			return err
		}
	}
	filter.tail = nil
	return nil
}

func suppressCodexExitSummary(output []byte) ([]byte, bool) {
	disconnected := bytes.Index(output, []byte("Disconnected from this task."))
	if disconnected < 0 {
		return output, false
	}
	start := disconnected
	if worked := bytes.LastIndex(output[:disconnected], []byte("Worked for ")); worked >= 0 {
		start = worked
		window := max(0, worked-128)
		if separator := bytes.LastIndex(output[window:worked], []byte("─")); separator >= 0 {
			candidate := window + separator
			if !bytes.ContainsAny(output[candidate:worked], "\r\n") {
				start = candidate
			}
		}
	}
	return output[:start], true
}

func writeAll(destination io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := destination.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}
