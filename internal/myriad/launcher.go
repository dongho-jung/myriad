package myriad

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type launcherRoute string

const (
	routeManaged      launcherRoute = "managed"
	routeManagedFresh launcherRoute = "managed-fresh"
	routeCurrent      launcherRoute = "current"
	routeResume       launcherRoute = "resume"
	routeDirect       launcherRoute = "direct"
	routeDescription  launcherRoute = "description"
)

func codexLauncherRoute(arguments []string, command []string) launcherRoute {
	subcommand := codexSubcommand(command)
	switch subcommand {
	case "review":
		return routeCurrent
	case "resume":
		if len(arguments) > 0 && arguments[0] == "resume" {
			return routeResume
		}
		return routeManagedFresh
	case "exec", "e", "apply", "a", "fork", "cloud", "sandbox":
		return routeManagedFresh
	case "agents", "archive", "app-server", "completion", "debug", "delete",
		"doctor", "exec-server", "features", "help", "login", "logout", "mcp", "mcp-server",
		"migrate-rollouts", "plugin", "queue", "remote-control", "unarchive", "update":
		return routeDirect
	}
	if len(arguments) == 0 {
		return routeManaged
	}
	if arguments[0] == "-h" || arguments[0] == "--help" || arguments[0] == "-V" || arguments[0] == "--version" {
		return routeDirect
	}
	if strings.HasPrefix(arguments[0], "-") {
		return routeManaged
	}
	return routeDescription
}

func directCLI(command []string, cwd string, environment []string) (int, error) {
	if len(command) == 0 {
		return 2, fail("empty direct command")
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = cwd
	cmd.Env = environment
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

func codexTUICommand(arguments []string, project string) []string {
	return append([]string{
		"codex",
		"-c", codexTrustedProjectsConfig([]string{project}),
		"-c", "tui.show_tooltips=false",
		"-c", `tui.status_line=["current-dir","thread-title","model-with-reasoning"]`,
		"--dangerously-bypass-approvals-and-sandbox",
	}, arguments...)
}

func selectedCodexProject(arguments []string) (string, bool) {
	project := currentDirectory()
	hasCD := false
	expect := false
	for _, argument := range arguments {
		if expect {
			project = argument
			hasCD = true
			expect = false
			continue
		}
		if argument == "--" {
			break
		}
		if argument == "-C" || argument == "--cd" {
			expect = true
		} else if strings.HasPrefix(argument, "--cd=") {
			project = strings.TrimPrefix(argument, "--cd=")
			hasCD = true
		}
	}
	if !filepath.IsAbs(project) {
		project = filepath.Join(currentDirectory(), project)
	}
	project, _ = canonical(project)
	return project, hasCD
}

func inGitRepository(path string) bool {
	_, err := repoRoot(path)
	return err == nil
}

func launchCodex(store *Store, arguments []string) (int, error) {
	project, hasCD := selectedCodexProject(arguments)
	if len(arguments) > 0 && arguments[0] == "--local" {
		arguments = arguments[1:]
		return directCLI(codexTUICommand(arguments, project), currentDirectory(), os.Environ())
	}
	if os.Getenv("MYRIAD_HARNESS") == "myriad" {
		return directCLI(codexTUICommand(arguments, project), currentDirectory(), os.Environ())
	}
	if !inGitRepository(currentDirectory()) {
		if hasCD {
			return openTaskCommand(store, launchOptions{Agent: "codex", Quiet: true, Command: codexTUICommand(arguments, project), LaunchCWD: currentDirectory()})
		}
		return directCLI(codexTUICommand(arguments, project), currentDirectory(), os.Environ())
	}
	if len(arguments) > 0 && arguments[0] == "resume" {
		return resumeTaskCommand(store, resumeOptions{Arguments: arguments[1:], Quiet: true})
	}
	if len(arguments) > 0 && arguments[0] == "--new" {
		remaining := arguments[1:]
		if len(remaining) == 0 {
			return openTaskCommand(store, launchOptions{Agent: "codex", New: true, Quiet: true})
		}
		first := remaining[0]
		if strings.HasPrefix(first, "-") || codexSubcommands[first] {
			return openTaskCommand(store, launchOptions{Agent: "codex", New: true, Quiet: true, Command: codexTUICommand(remaining, project)})
		}
		return openTaskCommand(store, launchOptions{Agent: "codex", New: true, Quiet: true, Description: strings.Join(remaining, " ")})
	}
	command := codexTUICommand(arguments, project)
	switch codexLauncherRoute(arguments, command) {
	case routeCurrent:
		return openTaskCommand(store, launchOptions{Agent: "codex", RequireCurrent: true, Quiet: true, Command: command})
	case routeResume:
		return resumeTaskCommand(store, resumeOptions{Arguments: arguments[1:], Quiet: true})
	case routeManagedFresh:
		return openTaskCommand(store, launchOptions{Agent: "codex", Fresh: true, Quiet: true, Command: command})
	case routeDirect:
		return directCLI(command, currentDirectory(), os.Environ())
	case routeDescription:
		return openTaskCommand(store, launchOptions{Agent: "codex", Quiet: true, Description: strings.Join(arguments, " ")})
	default:
		return openTaskCommand(store, launchOptions{Agent: "codex", Quiet: true, Command: command})
	}
}

func claudeCommand(arguments []string) []string {
	return append([]string{"env", "IS_DEMO=1", "claude", "--ide", "--chrome", "--allow-dangerously-skip-permissions", "--effort", "max", "--permission-mode", "bypassPermissions"}, arguments...)
}

func launchClaude(store *Store, arguments []string) (int, error) {
	if len(arguments) > 0 && arguments[0] == "--local" {
		return directCLI(claudeCommand(arguments[1:]), currentDirectory(), os.Environ())
	}
	if len(arguments) > 0 && arguments[0] == "agents" {
		return directCLI(claudeCommand(arguments), currentDirectory(), os.Environ())
	}
	ownsLifecycle := false
	for _, argument := range arguments {
		if argument == "--" {
			break
		}
		if argument == "--background" || argument == "--bg" || argument == "--tmux" || argument == "--worktree" || argument == "-w" || argument == "--cloud" || argument == "--environment" || argument == "--remote-control" || argument == "--teleport" || strings.HasPrefix(argument, "--background=") || strings.HasPrefix(argument, "--bg=") || strings.HasPrefix(argument, "--tmux=") || strings.HasPrefix(argument, "--worktree=") || strings.HasPrefix(argument, "--cloud=") || strings.HasPrefix(argument, "--environment=") || strings.HasPrefix(argument, "--remote-control=") || strings.HasPrefix(argument, "--teleport=") {
			ownsLifecycle = true
		}
	}
	if ownsLifecycle {
		if len(arguments) > 0 && arguments[0] == "--new" {
			arguments = arguments[1:]
		}
		return directCLI(claudeCommand(arguments), currentDirectory(), os.Environ())
	}
	if !inGitRepository(currentDirectory()) || os.Getenv("MYRIAD_HARNESS") == "myriad" {
		return directCLI(claudeCommand(arguments), currentDirectory(), os.Environ())
	}
	if len(arguments) > 0 && arguments[0] == "--new" {
		remaining := arguments[1:]
		if len(remaining) > 0 && (remaining[0] == "ultrareview" || strings.HasPrefix(remaining[0], "-")) {
			return openTaskCommand(store, launchOptions{Agent: "claude", New: true, Quiet: true, Command: claudeCommand(remaining)})
		}
		return openTaskCommand(store, launchOptions{Agent: "claude", New: true, Quiet: true, Description: strings.Join(remaining, " ")})
	}
	if len(arguments) > 0 {
		first := arguments[0]
		if first == "ultrareview" {
			return openTaskCommand(store, launchOptions{Agent: "claude", RequireCurrent: true, Quiet: true, Command: claudeCommand(arguments)})
		}
		if map[string]bool{"auth": true, "auto-mode": true, "doctor": true, "gateway": true, "import": true, "install": true, "mcp": true, "plugin": true, "plugins": true, "project": true, "setup-token": true, "update": true, "upgrade": true, "-h": true, "--help": true, "-v": true, "--version": true}[first] {
			return directCLI(claudeCommand(arguments), currentDirectory(), os.Environ())
		}
		if strings.HasPrefix(first, "-") {
			return openTaskCommand(store, launchOptions{Agent: "claude", Quiet: true, Command: claudeCommand(arguments)})
		}
		return openTaskCommand(store, launchOptions{Agent: "claude", Quiet: true, Description: strings.Join(arguments, " ")})
	}
	return openTaskCommand(store, launchOptions{Agent: "claude", Quiet: true})
}
