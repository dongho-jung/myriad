package myriad

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func superviseAgent(command []string, descriptors []int, sessionPath, sessionID string, foregroundPGID int) (int, error) {
	defer closeDescriptors(descriptors)
	metadata := Record{}
	store, err := NewStore()
	if err != nil {
		return 2, err
	}
	workingDirectory := currentDirectory()
	controlSocket := ""
	if sessionPath != "" {
		if err := transferCheckoutSessionOwner(sessionPath, sessionID, os.Getpid()); err != nil {
			return 2, err
		}
		if err := readJSON(sessionPath, maxJSONBytes, &metadata); err != nil {
			return 2, err
		}
		if taskID := os.Getenv("MYRIAD_TASK_ID"); taskID != "" && stringValue(metadata, "task_id") == taskID {
			start := processStart(os.Getpid())
			deadline := time.Now().Add(5 * time.Second)
			for {
				task, loadErr := store.Load(taskID)
				if loadErr == nil && taskProcessMatches(task, os.Getpid(), start) {
					break
				}
				if time.Now().After(deadline) {
					return 2, fail("managed task did not register its lock supervisor before launch")
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		workingDirectory = firstNonempty(stringValue(metadata, "working_directory"), workingDirectory)
		controlSocket = stringValue(metadata, "control_socket")
		_ = os.Setenv(envAgentSessionPath, sessionPath)
		_ = os.Setenv(envAgentSessionID, sessionID)
	}

	var control *codexServer
	if controlSocket != "" {
		provisionRequired := codexProvisionHookRequired()
		server, transformed, startErr := startCodexAppServer(
			store, command, controlSocket,
			[]string{currentDirectory(), workingDirectory}, os.Environ(),
		)
		if startErr != nil {
			_ = updateSessionMetadata(sessionPath, sessionID, Record{"control_status": "unavailable", "control_error": startErr.Error()})
			if provisionRequired {
				return 127, fail("Codex provisioning bridge is unavailable; refusing to launch in the original checkout: %v", startErr)
			}
			fmt.Fprintf(os.Stderr, "myriad: Codex notification bridge unavailable: %v\n", startErr)
		} else if server != nil {
			control = server
			command = transformed
			updates := Record{"control_status": "ready", "control_started_at": now()}
			_ = updateSessionMetadata(sessionPath, sessionID, updates)
		}
	}

	agent := exec.Command(command[0], command[1:]...)
	agent.Env = os.Environ()
	agent.Stdin = os.Stdin
	agent.Stdout = os.Stdout
	agent.Stderr = os.Stderr
	agent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: foregroundPGID, Pdeathsig: syscall.SIGKILL}
	if err := agent.Start(); err != nil {
		if control != nil {
			stopCodexServer(control, true)
		}
		return 127, fail("cannot execute supervised agent: %v", err)
	}
	agentPID := agent.Process.Pid

	signalChannel := make(chan os.Signal, 32)
	signal.Notify(signalChannel, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2)
	defer signal.Stop(signalChannel)
	if sessionPath != "" {
		_ = updateSessionMetadata(sessionPath, sessionID, Record{
			"notification_ready": true, "notification_state": "ready", "notification_ready_at": now(),
		})
	}

	mainDone := false
	mainExit := 127
	controlDone := control == nil
	intentionalHandoff := false
	notificationRequested := sessionPath != ""
	nextNotificationRetry := time.Time{}
	handoffAt := time.Time{}
	handoffForceAt := time.Time{}
	descendantTerminateAt := time.Time{}
	controlTerminateAt := time.Time{}
	controlKilled := false
	codexTitleFinalized := control == nil
	notificationClosed := false

	for {
		for {
			var status unix.WaitStatus
			pid, waitErr := unix.Wait4(-1, &status, unix.WNOHANG, nil)
			if waitErr != nil {
				if errors.Is(waitErr, unix.EINTR) {
					continue
				}
				if errors.Is(waitErr, unix.ECHILD) && mainDone {
					return supervisorResult(intentionalHandoff, mainExit), nil
				}
				break
			}
			if pid <= 0 {
				break
			}
			if pid == agentPID {
				mainDone = true
				mainExit = statusExitCode(status)
				_ = agent.Process.Release()
			} else if control != nil && pid == control.Command.Process.Pid {
				controlDone = true
				_ = control.Command.Process.Release()
			}
		}

		if mainDone && sessionPath != "" && !notificationClosed {
			_ = updateSessionMetadata(sessionPath, sessionID, Record{"notification_ready": false, "notification_state": "closed", "notification_closed_at": now()})
			notificationClosed = true
		}

		if sessionPath != "" && !mainDone {
			if notificationRequested || (!nextNotificationRetry.IsZero() && time.Now().After(nextNotificationRetry)) {
				notificationRequested = false
				pending, pendingErr := pendingInboxMessages(store, sessionID, false)
				if pendingErr == nil && len(pending) > 0 {
					if control != nil && !controlDone {
						if err := deliverPendingCodexNotifications(store, sessionID, controlSocket, workingDirectory); err != nil {
							nextNotificationRetry = time.Now().Add(10 * time.Second)
							if !interactiveCodexCommand(command) {
								terminalInboxAlert(sessionID, len(pending))
							}
							_ = updateSessionMetadata(sessionPath, sessionID, Record{"last_notification_error": err.Error()})
						} else {
							nextNotificationRetry = time.Time{}
						}
					} else {
						terminalInboxAlert(sessionID, len(pending))
						nextNotificationRetry = time.Time{}
					}
				}
			}
			if intentionalHandoff && !handoffAt.IsZero() && time.Now().After(handoffAt) {
				tasks, _ := acceptedHandoffTasks(store, sessionID)
				_ = updateSessionMetadata(sessionPath, sessionID, Record{"handoff_task_ids": stringsToAny(tasks)})
				signal := unix.SIGTERM
				if interactiveCodexCommand(command) {
					signal = unix.SIGINT
					handoffForceAt = time.Now().Add(2 * time.Second)
				}
				_ = unix.Kill(agentPID, signal)
				handoffAt = time.Time{}
			}
			if intentionalHandoff && !handoffForceAt.IsZero() && time.Now().After(handoffForceAt) {
				_ = unix.Kill(agentPID, unix.SIGTERM)
				handoffForceAt = time.Time{}
			}
		}

		if mainDone && control != nil && !controlDone && !codexTitleFinalized {
			if err := finalizeCodexThreadName(store, sessionPath, sessionID, controlSocket, workingDirectory, true); err != nil {
				fmt.Fprintf(os.Stderr, "myriad: Codex final title unavailable: %v\n", err)
				if sessionPath != "" {
					_ = updateSessionMetadata(sessionPath, sessionID, Record{
						"codex_thread_name_finalize_error": err.Error(),
					})
				}
			}
			codexTitleFinalized = true
		}

		if mainDone && control != nil && !controlDone {
			if controlTerminateAt.IsZero() {
				_ = unix.Kill(-control.Command.Process.Pid, unix.SIGTERM)
				controlTerminateAt = time.Now().Add(2 * time.Second)
			} else if !controlKilled && time.Now().After(controlTerminateAt) {
				_ = unix.Kill(-control.Command.Process.Pid, unix.SIGKILL)
				controlKilled = true
			}
		}

		if mainDone {
			descendants := []int{}
			for _, child := range directChildPIDs(os.Getpid()) {
				if control != nil && child == control.Command.Process.Pid && !controlDone {
					continue
				}
				descendants = append(descendants, child)
			}
			if len(descendants) > 0 && descendantTerminateAt.IsZero() {
				for _, child := range descendants {
					_ = unix.Kill(child, unix.SIGTERM)
				}
				descendantTerminateAt = time.Now().Add(time.Second)
			} else if len(descendants) > 0 && time.Now().After(descendantTerminateAt) {
				for _, child := range descendants {
					_ = unix.Kill(child, unix.SIGKILL)
				}
			} else if len(descendants) == 0 {
				descendantTerminateAt = time.Time{}
			}
		}

		select {
		case received := <-signalChannel:
			switch received {
			case syscall.SIGUSR1:
				notificationRequested = true
			case syscall.SIGUSR2:
				if sessionPath != "" && !intentionalHandoff {
					tasks, _ := acceptedHandoffTasks(store, sessionID)
					if len(tasks) > 0 {
						intentionalHandoff = true
						handoffAt = time.Now().Add(500 * time.Millisecond)
						_ = updateSessionMetadata(sessionPath, sessionID, Record{"handoff_task_ids": stringsToAny(tasks), "handoff_started_at": now()})
					}
				}
			default:
				if !mainDone {
					if signalValue, ok := received.(syscall.Signal); ok {
						_ = unix.Kill(agentPID, signalValue)
					}
				}
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func supervisorResult(intentional bool, mainExit int) int {
	if intentional {
		return handoffExitCode
	}
	return mainExit
}

func terminalInboxAlert(sessionID string, count int) {
	message := fmt.Sprintf("\r\n\a[myriad] %d integration handoff event(s) are waiting in session %s. Run: myriad inbox\r\n", count, sessionID)
	terminal, err := os.OpenFile("/dev/tty", os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		fmt.Fprint(os.Stderr, message)
		return
	}
	defer func() { _ = terminal.Close() }()
	_, _ = terminal.WriteString(message)
}
