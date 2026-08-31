package myriad

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type supervisedResult struct {
	ExitCode      int
	SupervisorPID int
}

func runSupervised(command []string, cwd string, environment []string, reservation *checkoutReservation, started func(Record) error) (supervisedResult, error) {
	if len(command) == 0 {
		return supervisedResult{}, fail("supervisor has no agent command")
	}
	executable, err := executablePath()
	if err != nil {
		return supervisedResult{}, err
	}
	arguments := append([]string{internalSupervise, "--"}, command...)
	cmd := exec.Command(executable, arguments...)
	cmd.Dir = cwd
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = environment
	foregroundPGID, err := unix.Getpgid(0)
	if err != nil || foregroundPGID <= 0 {
		return supervisedResult{}, fail("cannot identify foreground process group")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = overlayEnvironment(cmd.Env, map[string]string{envForegroundPGID: strconv.Itoa(foregroundPGID)})
	files := reservation.Files()
	if len(files) > 0 {
		cmd.ExtraFiles = files
		fds := make([]string, len(files))
		for index := range files {
			fds[index] = strconv.Itoa(3 + index)
		}
		cmd.Env = overlayEnvironment(cmd.Env, map[string]string{envInheritedLockFDs: strings.Join(fds, ",")})
	}
	if reservation != nil && reservation.SessionPath != "" {
		cmd.Env = overlayEnvironment(cmd.Env, map[string]string{
			envLockSessionPath: reservation.SessionPath,
			envLockSessionID:   reservation.SessionID,
		})
	}
	if err := cmd.Start(); err != nil {
		return supervisedResult{}, fail("cannot launch supervisor: %v", err)
	}
	identity := processRecord(cmd.Process.Pid, "lock-supervisor", 0)
	if identity == nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return supervisedResult{}, fail("cannot record Myriad supervisor identity")
	}
	if started != nil {
		if err := started(identity); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return supervisedResult{}, err
		}
	}
	// The launcher remains in the agent's foreground group while the lock
	// supervisor runs in its own group. Capture terminal signals here so the
	// launcher can finish bookkeeping after the agent exits.
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case waitErr := <-done:
			return supervisedResult{ExitCode: exitCode(waitErr), SupervisorPID: cmd.Process.Pid}, nil
		case <-signals:
			// The foreground agent already received the terminal signal. Keep the
			// launcher alive long enough to record and finalize its result.
		}
	}
}

func supervisor(raw []string) (int, error) {
	if err := platformSupported(); err != nil {
		return 2, err
	}
	command := append([]string{}, raw...)
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	if len(command) == 0 {
		return 2, fail("supervisor has no agent command")
	}
	sessionPath := os.Getenv(envLockSessionPath)
	sessionID := os.Getenv(envLockSessionID)
	foregroundPGID, err := strconv.Atoi(os.Getenv(envForegroundPGID))
	if err != nil || foregroundPGID <= 0 {
		return 2, fail("supervisor received an invalid foreground process group")
	}
	if (sessionPath == "") != (sessionID == "") {
		return 2, fail("supervisor received incomplete session metadata")
	}
	descriptors, err := inheritedDescriptors()
	if err != nil {
		return 2, err
	}
	if len(descriptors) == 0 {
		return 2, fail("supervisor received no checkout lease")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		closeDescriptors(descriptors)
		return 2, fail("cannot enable descendant supervision: %v", err)
	}
	_ = os.Unsetenv(envInheritedLockFDs)
	_ = os.Unsetenv(envLockSessionPath)
	_ = os.Unsetenv(envLockSessionID)
	_ = os.Unsetenv(envForegroundPGID)
	return superviseAgent(command, descriptors, sessionPath, sessionID, foregroundPGID)
}

func inheritedDescriptors() ([]int, error) {
	encoded := os.Getenv(envInheritedLockFDs)
	result := []int{}
	for _, value := range strings.Split(encoded, ",") {
		if value == "" {
			continue
		}
		descriptor, err := strconv.Atoi(value)
		if err != nil || descriptor < 3 {
			return nil, fail("supervisor received invalid descriptors")
		}
		result = append(result, descriptor)
	}
	return result, nil
}

func gracefulCodexInterrupt(agent string, command []string, exitCode int) bool {
	return exitCode == 130 && agent == "codex" && interactiveCodexCommand(command)
}

func defaultAgentCommand(agent, prompt string) ([]string, error) {
	var result []string
	switch agent {
	case "codex":
		result = []string{"codex", "--dangerously-bypass-approvals-and-sandbox"}
	case "claude":
		result = []string{"env", "IS_DEMO=1", "claude", "--ide", "--chrome", "--allow-dangerously-skip-permissions", "--effort", "max", "--permission-mode", "bypassPermissions"}
	default:
		return nil, fail("custom agents require a command after --")
	}
	if prompt != "" {
		result = append(result, prompt)
	}
	return result, nil
}

func validateForegroundAgentCommand(agent string, command []string, lockManaged bool) error {
	if !lockManaged {
		return nil
	}
	codex := commandExecutableIndex(command, "codex")
	claudeExecutable := commandExecutableIndex(command, "claude")
	if agent == "codex" && codex < 0 {
		return fail("managed Codex task requires a directly identifiable codex executable")
	}
	if agent == "claude" && claudeExecutable < 0 {
		return fail("managed Claude task requires a directly identifiable claude executable")
	}
	if codexUsesRemoteAppServer(command) {
		return fail("Codex --remote uses an external App Server lifecycle and cannot run inside a managed Myriad task")
	}
	claude := agent == "claude" || claudeExecutable >= 0
	if !claude {
		return nil
	}
	if claudeExecutable >= 0 && claudeDirectInvocation(command[claudeExecutable+1:]) {
		return fail("this Claude command owns a separate session lifecycle and cannot run inside a managed Myriad task")
	}
	for _, value := range command {
		if value == "--background" || value == "--bg" || value == "--tmux" || value == "--worktree" || value == "-w" || strings.HasPrefix(value, "--tmux=") || strings.HasPrefix(value, "--worktree=") {
			return fail("Claude background, tmux, and built-in worktree modes own a separate lifecycle; launch them directly with `myriad claude` instead of nesting them in a Myriad worktree")
		}
	}
	return nil
}

func managedAgentWorkingDirectory(task Record, command []string) (string, error) {
	if interactiveCodexCommand(command) {
		return taskOriginWorkingDirectory(task)
	}
	return taskWorkingDirectory(task), nil
}

func taskProcessMatches(task Record, pid int, start string) bool {
	owner := recordMap(task, "process")
	recordedPID, _ := intValue(owner["pid"])
	return recordedPID == pid && stringValue(owner, "start") == start
}

func taskLaunch(store *Store, task Record, command []string, reservation *checkoutReservation) (int, error) {
	if err := validateForegroundAgentCommand(stringValue(task, "agent"), command, true); err != nil {
		return 2, err
	}
	agentCommand := managedAgentCommand(task, command)
	cwd, err := managedAgentWorkingDirectory(task, agentCommand)
	if err != nil {
		return 2, err
	}
	delete(task, "process")
	if err := store.Save(task); err != nil {
		return 2, err
	}
	result, err := runSupervised(agentCommand, cwd, taskEnvironment(task), reservation, func(identity Record) error {
		task["process"] = identity
		return setStatus(store, task, StatusRunning, "")
	})
	if err != nil {
		delete(task, "process")
		_ = store.Save(task)
		return 2, err
	}
	if !taskWorktreeReady(task) {
		current, loadErr := store.Load(stringValue(task, "task_id"))
		if loadErr == nil {
			task = replaceRecord(task, current)
		}
	}
	delete(task, "process")
	recordAgentExit(task, result.ExitCode, gracefulCodexInterrupt(stringValue(task, "agent"), agentCommand, result.ExitCode))
	if err := store.Save(task); err != nil {
		return result.ExitCode, err
	}
	return result.ExitCode, nil
}

func replaceRecord(destination, source Record) Record {
	for key := range destination {
		delete(destination, key)
	}
	for key, value := range source {
		destination[key] = value
	}
	return destination
}

func launchNative(agent, description string, command []string, cwd string, reservation *checkoutReservation) (int, error) {
	if len(command) == 0 {
		var err error
		command, err = defaultAgentCommand(agent, description)
		if err != nil {
			return 2, err
		}
	}
	if reservation == nil {
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Dir = cwd
		cmd.Env = nativeAgentEnvironment()
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err == nil {
			return 0, nil
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil
		}
		return 127, fail("cannot execute %s: %v", displayCommand(command), err)
	}
	if err := validateForegroundAgentCommand(agent, command, true); err != nil {
		return 2, err
	}
	result, err := runSupervised(command, cwd, nativeAgentEnvironment(), reservation, nil)
	return result.ExitCode, err
}

func directChildPIDs(pid int) []int {
	payload, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if err != nil {
		return nil
	}
	result := []int{}
	for _, value := range strings.Fields(string(payload)) {
		child, err := strconv.Atoi(value)
		if err == nil && child > 1 {
			result = append(result, child)
		}
	}
	return result
}

func statusExitCode(status unix.WaitStatus) int {
	if status.Exited() {
		return status.ExitStatus()
	}
	if status.Signaled() {
		return 128 + int(status.Signal())
	}
	return 127
}

func processIdentityEqual(left, right Record) bool {
	leftPID, _ := intValue(left["pid"])
	rightPID, _ := intValue(right["pid"])
	return leftPID == rightPID && stringValue(left, "start") != "" && stringValue(left, "start") == stringValue(right, "start")
}

func closeDescriptors(descriptors []int) {
	for _, descriptor := range descriptors {
		_ = unix.Close(descriptor)
	}
}
