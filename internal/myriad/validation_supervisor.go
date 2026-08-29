package myriad

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// validationSupervisor gives every validation command a subreaper boundary.
// A check is complete only after its main process and every descendant are
// gone, including children that detached into another process group/session.
func validationSupervisor(raw []string) (int, error) {
	if err := platformSupported(); err != nil {
		return 2, err
	}
	command := append([]string{}, raw...)
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	if len(command) == 0 {
		return 2, fail("validation supervisor has no command")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return 2, fail("cannot supervise validation descendants: %v", err)
	}
	check := exec.Command(command[0], command[1:]...)
	check.Env = os.Environ()
	check.Stdin = os.Stdin
	check.Stdout = os.Stdout
	check.Stderr = os.Stderr
	if err := check.Start(); err != nil {
		return 127, fail("cannot execute validation command: %v", err)
	}
	mainPID := check.Process.Pid
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	mainDone := false
	mainExit := 127
	terminationDeadline := time.Time{}
	descendantDeadline := time.Time{}
	for {
		noChildren := false
		for {
			var status unix.WaitStatus
			pid, waitErr := unix.Wait4(-1, &status, unix.WNOHANG, nil)
			if waitErr != nil {
				if errors.Is(waitErr, unix.EINTR) {
					continue
				}
				noChildren = errors.Is(waitErr, unix.ECHILD)
				break
			}
			if pid <= 0 {
				break
			}
			if pid == mainPID {
				mainDone = true
				mainExit = statusExitCode(status)
				_ = check.Process.Release()
			}
		}
		if mainDone && noChildren {
			return mainExit, nil
		}

		if !mainDone && !terminationDeadline.IsZero() && time.Now().After(terminationDeadline) {
			_ = unix.Kill(mainPID, unix.SIGKILL)
			terminationDeadline = time.Time{}
		}
		if mainDone {
			descendants := directChildPIDs(os.Getpid())
			if len(descendants) == 0 {
				descendantDeadline = time.Time{}
			} else if descendantDeadline.IsZero() {
				for _, pid := range descendants {
					_ = unix.Kill(pid, unix.SIGTERM)
				}
				descendantDeadline = time.Now().Add(time.Second)
			} else if time.Now().After(descendantDeadline) {
				for _, pid := range descendants {
					_ = unix.Kill(pid, unix.SIGKILL)
				}
			}
		}

		select {
		case received := <-signals:
			if !mainDone {
				signalValue := unix.SIGTERM
				if value, ok := received.(syscall.Signal); ok {
					signalValue = value
				}
				_ = unix.Kill(mainPID, signalValue)
				terminationDeadline = time.Now().Add(2 * time.Second)
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
}
