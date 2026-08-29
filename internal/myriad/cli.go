package myriad

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

var Version = "dev"

func Run(arguments []string) int {
	if len(arguments) > 0 {
		switch arguments[0] {
		case internalSupervise:
			code, err := supervisor(arguments[1:])
			return finish(code, err)
		case internalValidate:
			code, err := validationSupervisor(arguments[1:])
			return finish(code, err)
		case internalLease:
			code, err := attachmentLease()
			return finish(code, err)
		case internalInboxHook:
			return finish(0, inboxHook())
		case internalProvision:
			return finish(0, provisionHook())
		}
	}
	if len(arguments) == 0 || arguments[0] == "help" || arguments[0] == "--help" || arguments[0] == "-h" {
		printHelp()
		return 0
	}
	if arguments[0] == "version" || arguments[0] == "--version" || arguments[0] == "-V" {
		fmt.Printf("myriad %s\n", Version)
		return 0
	}
	store, err := NewStore()
	if err != nil {
		if arguments[0] == "statusline" {
			return 0
		}
		return finish(2, err)
	}
	var code int
	switch arguments[0] {
	case "codex":
		code, err = launchCodex(store, arguments[1:])
	case "claude":
		code, err = launchClaude(store, arguments[1:])
	case "open", "start":
		var options launchOptions
		options, err = parseLaunchOptions(arguments[1:])
		if err == nil {
			if arguments[0] == "open" {
				code, err = openTaskCommand(store, options)
			} else {
				code, err = startTaskCommand(store, options)
			}
		}
	case "resume":
		var options resumeOptions
		options, err = parseResumeOptions(arguments[1:])
		if err == nil {
			code, err = resumeTaskCommand(store, options)
		}
	case "context":
		err = runContextCommand(store, arguments[1:])
	case "list":
		printTaskList(store)
	case "status":
		if len(arguments) == 1 {
			printTaskList(store)
		} else if len(arguments) == 2 {
			var task Record
			task, err = store.Load(arguments[1])
			if err == nil {
				payload, _ := json.MarshalIndent(task, "", "  ")
				fmt.Println(string(payload))
			}
		} else {
			err = fail("usage: myriad status [TASK_ID]")
		}
	case "statusline":
		err = runStatusline(store, arguments[1:])
	case "publish":
		if len(arguments) > 2 {
			err = fail("usage: myriad publish [TASK_ID]")
		} else {
			selected := ""
			if len(arguments) == 2 {
				selected = arguments[1]
			}
			code, err = publishCommand(store, selected)
		}
	case "integrate":
		if len(arguments) != 2 {
			err = fail("usage: myriad integrate TASK_ID")
		} else {
			code, err = integrateTaskCommand(store, arguments[1], false)
		}
	case "recover":
		code, err = runRecoverCommand(store, arguments[1:])
	case "cleanup":
		if len(arguments) == 2 && arguments[1] == "--all" {
			code = cleanupCommand(store, "", true)
		} else if len(arguments) == 2 {
			code = cleanupCommand(store, arguments[1], false)
		} else {
			err = fail("usage: myriad cleanup TASK_ID | --all")
		}
	case "reconcile":
		integrate, quiet := true, false
		for _, argument := range arguments[1:] {
			if argument == "--no-integrate" {
				integrate = false
			} else if argument == "--quiet" {
				quiet = true
			} else {
				err = fail("unknown reconcile option: %s", argument)
			}
		}
		if err == nil {
			code = reconcile(store, integrate, quiet)
		}
	case "attach":
		if len(arguments) != 2 {
			err = fail("usage: myriad attach PATH")
		} else {
			err = attachRepository(store, arguments[1])
		}
	case "inbox":
		err = runInboxCommand(store, arguments[1:])
	case "handoff":
		if len(arguments) != 2 {
			err = fail("usage: myriad handoff EVENT_ID")
		} else {
			err = handoffCommand(store, arguments[1])
		}
	default:
		err = fail("unknown command %q; run `myriad help`", arguments[0])
	}
	return finish(code, err)
}

func finish(code int, err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, "myriad:", err)
		if code == 0 {
			return 2
		}
	}
	return code
}

func splitCommand(arguments []string) ([]string, []string) {
	for index, argument := range arguments {
		if argument == "--" {
			return arguments[:index], arguments[index+1:]
		}
	}
	return arguments, nil
}

func requireOptionValue(arguments []string, index *int, option string) (string, error) {
	if *index+1 >= len(arguments) {
		return "", fail("%s requires a value", option)
	}
	*index = *index + 1
	return arguments[*index], nil
}

func parseLaunchOptions(arguments []string) (launchOptions, error) {
	options := launchOptions{Agent: "codex", CheckTimeout: defaultCheckTimeout.Seconds(), LaunchCWD: currentDirectory()}
	flags, command := splitCommand(arguments)
	options.Command = command
	positionals := []string{}
	for index := 0; index < len(flags); index++ {
		argument := flags[index]
		switch argument {
		case "--agent", "--target", "--check", "--check-timeout", "--task":
			value, err := requireOptionValue(flags, &index, argument)
			if err != nil {
				return options, err
			}
			switch argument {
			case "--agent":
				if value != "codex" && value != "claude" && value != "custom" {
					return options, fail("unsupported agent: %s", value)
				}
				options.Agent = value
			case "--target":
				options.Target = value
			case "--check":
				options.Checks = append(options.Checks, value)
			case "--check-timeout":
				parsed, err := strconv.ParseFloat(value, 64)
				if err != nil || parsed <= 0 {
					return options, fail("--check-timeout must be greater than zero")
				}
				options.CheckTimeout = parsed
			case "--task":
				options.Task = value
			}
		case "--no-integrate":
			options.NoIntegrate = true
		case "--new":
			options.New = true
		case "--fresh":
			options.Fresh = true
		case "--local":
			options.Local = true
		case "--quiet":
			options.Quiet = true
		case "--require-current":
			options.RequireCurrent = true
		case "--new-session":
			options.NewSession = true
		default:
			if strings.HasPrefix(argument, "-") {
				return options, fail("unknown launch option: %s", argument)
			}
			positionals = append(positionals, argument)
		}
	}
	if len(positionals) > 1 {
		return options, fail("launch description must be one quoted argument")
	}
	if len(positionals) == 1 {
		options.Description = positionals[0]
	}
	return options, nil
}

func parseResumeOptions(arguments []string) (resumeOptions, error) {
	options := resumeOptions{CheckTimeout: defaultCheckTimeout.Seconds()}
	flags, command := splitCommand(arguments)
	options.Arguments = command
	for index := 0; index < len(flags); index++ {
		argument := flags[index]
		switch argument {
		case "--last":
			options.Last = true
		case "--all":
			options.All = true
		case "--include-non-interactive":
			options.IncludeNonInteractive = true
		case "--no-integrate":
			options.NoIntegrate = true
		case "--quiet":
			options.Quiet = true
		case "--target", "--check", "--check-timeout":
			value, err := requireOptionValue(flags, &index, argument)
			if err != nil {
				return options, err
			}
			if argument == "--target" {
				options.Target = value
			} else if argument == "--check" {
				options.Checks = append(options.Checks, value)
			} else {
				parsed, err := strconv.ParseFloat(value, 64)
				if err != nil || parsed <= 0 {
					return options, fail("--check-timeout must be greater than zero")
				}
				options.CheckTimeout = parsed
			}
		default:
			if strings.HasPrefix(argument, "-") {
				return options, fail("unknown resume option: %s", argument)
			}
			if options.SessionID != "" {
				return options, fail("resume accepts only one session id")
			}
			options.SessionID = argument
		}
	}
	return options, nil
}

func runContextCommand(store *Store, arguments []string) error {
	var taskID, jira, pr string
	clearJira, clearPR, actions := false, false, 0
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "--task", "--jira", "--pr":
			value, err := requireOptionValue(arguments, &index, arguments[index])
			if err != nil {
				return err
			}
			if arguments[index-1] == "--task" {
				taskID = value
			} else if arguments[index-1] == "--jira" {
				jira = value
				actions++
			} else {
				pr = value
				actions++
			}
		case "--clear-jira":
			clearJira = true
			actions++
		case "--clear-pr":
			clearPR = true
			actions++
		default:
			return fail("unknown context option: %s", arguments[index])
		}
	}
	if actions != 1 {
		return fail("context requires exactly one of --jira, --clear-jira, --pr, --clear-pr")
	}
	return contextCommand(store, taskID, jira, pr, clearJira, clearPR)
}

func runStatusline(store *Store, arguments []string) error {
	claude, width, epoch := false, 100, float64(time.Now().Unix())
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "--claude":
			claude = true
		case "--width", "--epoch":
			option := arguments[index]
			value, err := requireOptionValue(arguments, &index, option)
			if err != nil {
				return err
			}
			if option == "--width" {
				width, err = strconv.Atoi(value)
				if err != nil || width <= 0 {
					return fail("--width must be greater than zero")
				}
			} else {
				epoch, err = strconv.ParseFloat(value, 64)
				if err != nil {
					return fail("invalid --epoch")
				}
			}
		default:
			return fail("unknown statusline option: %s", arguments[index])
		}
	}
	payload := Record{}
	if claude {
		raw, _ := io.ReadAll(io.LimitReader(os.Stdin, maxHookInputBytes+1))
		if len(raw) <= maxHookInputBytes && len(strings.TrimSpace(string(raw))) > 0 {
			_ = decodeJSON(raw, &payload)
		}
	}
	if line := statusline(store, payload, claude, width, epoch); line != "" {
		fmt.Println(line)
	}
	return nil
}

func runRecoverCommand(store *Store, arguments []string) (int, error) {
	flags, command := splitCommand(arguments)
	if len(flags) == 0 {
		return 2, fail("usage: myriad recover TASK_ID [options] [-- COMMAND]")
	}
	taskID := flags[0]
	agent, prompt := "", ""
	newSession, quiet := false, false
	var policy *bool
	for index := 1; index < len(flags); index++ {
		switch flags[index] {
		case "--agent", "--prompt":
			option := flags[index]
			value, err := requireOptionValue(flags, &index, option)
			if err != nil {
				return 2, err
			}
			if option == "--agent" {
				agent = value
			} else {
				prompt = value
			}
		case "--integrate":
			value := true
			policy = &value
		case "--no-integrate":
			value := false
			policy = &value
		case "--new-session":
			newSession = true
		case "--quiet":
			quiet = true
		default:
			return 2, fail("unknown recover option: %s", flags[index])
		}
	}
	return recoverTask(store, taskID, agent, policy, newSession, prompt, command, quiet)
}

func runInboxCommand(store *Store, arguments []string) error {
	sessionID, eventID := "", ""
	asJSON := false
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "--session":
			value, err := requireOptionValue(arguments, &index, "--session")
			if err != nil {
				return err
			}
			sessionID = value
		case "--json":
			asJSON = true
		default:
			if strings.HasPrefix(arguments[index], "-") || eventID != "" {
				return fail("invalid inbox argument: %s", arguments[index])
			}
			eventID = arguments[index]
		}
	}
	return inboxCommand(store, sessionID, eventID, asJSON)
}

func printHelp() {
	fmt.Print(`Myriad coordinates coding-agent sessions and isolated Git worktrees.

Usage:
  myriad codex [ARGS...]              launch Codex through Myriad
  myriad claude [ARGS...]             launch Claude through Myriad
  myriad open [OPTIONS] [TASK]        resume or create managed work
  myriad resume [OPTIONS] [SESSION]   resume work or a saved Codex chat
  myriad start [OPTIONS] [TASK]       create a managed task directly
  myriad publish [TASK_ID]            publish an active checkpoint
  myriad attach PATH                  attach another repository
  myriad context ACTION               set Jira/PR display context
  myriad list | status [TASK_ID]      inspect local lifecycle state
  myriad inbox | handoff EVENT_ID     coordinate queued integration
  myriad integrate TASK_ID            retry integration
  myriad recover TASK_ID              resume preserved work
  myriad cleanup TASK_ID|--all        remove safe inactive worktrees
  myriad reconcile [--quiet]          repair interrupted lifecycle state
  myriad statusline [--claude]        render active worktrees
  myriad version                      print the binary version

Run agents with --local to bypass Myriad explicitly. Validation commands passed
with --check are parsed as argv and run directly; shell pipelines are not used.
`)
}
