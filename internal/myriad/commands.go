package myriad

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type launchOptions struct {
	Description    string
	Task           string
	Agent          string
	Target         string
	Checks         []string
	CheckTimeout   float64
	NoIntegrate    bool
	New            bool
	Fresh          bool
	Local          bool
	Quiet          bool
	RequireCurrent bool
	NewSession     bool
	Command        []string
	LaunchCWD      string
}

func prepareLaunch(options *launchOptions) error {
	if options.Agent == "" {
		options.Agent = "codex"
	}
	if options.LaunchCWD == "" {
		options.LaunchCWD = currentDirectory()
	}
	command, selected, err := normalizeCodexWorkingDirectory(options.Command, options.LaunchCWD)
	if err != nil {
		return err
	}
	options.Command = command
	options.LaunchCWD = selected
	return nil
}

func startTaskCommand(store *Store, options launchOptions) (int, error) {
	if err := prepareLaunch(&options); err != nil {
		return 2, err
	}
	command := append([]string{}, options.Command...)
	if len(command) == 0 {
		var err error
		command, err = defaultAgentCommand(options.Agent, options.Description)
		if err != nil {
			return 2, err
		}
	}
	if err := validateForegroundAgentCommand(options.Agent, command, true); err != nil {
		return 2, err
	}
	checkout, err := repoRoot(options.LaunchCWD)
	if err != nil {
		return 2, err
	}
	repository, err := primaryWorktree(checkout)
	if err != nil {
		return 2, err
	}
	activity, err := store.RepositoryActivityLock(repository, false, true)
	if err != nil {
		return 2, err
	}
	description := firstNonempty(options.Task, options.Description)
	task, createErr := createTask(store, createTaskOptions{
		LaunchCWD: options.LaunchCWD, Agent: options.Agent, Target: options.Target,
		Checks: options.Checks, CheckTimeout: options.CheckTimeout,
		NoIntegrate: options.NoIntegrate, Description: description,
		Quiet: options.Quiet, Deferred: interactiveCodexCommand(command),
	})
	_ = activity.Unlock()
	if createErr != nil {
		return 2, createErr
	}
	if !options.Quiet {
		branch := firstNonempty(stringValue(task, "branch"), "pending (first prompt)")
		fmt.Printf("task: %s\nworktree: %s\nbranch: %s\n", stringValue(task, "task_id"), stringValue(task, "worktree_path"), branch)
	}
	exitCode, err := launchForTask(store, task, command, boolValue(task, "auto_integrate", true), false)
	if !options.Quiet {
		fmt.Printf("status: %s\n", stringValue(task, "status"))
		if reason := stringValue(task, "status_reason"); reason != "" {
			fmt.Printf("reason: %s\n", reason)
		}
		if result := stringValue(task, "result_commit"); result != "" {
			fmt.Printf("result: %s\n", result)
		}
		if integrated := stringValue(task, "integrated_commit"); integrated != "" {
			fmt.Printf("integrated: %s -> %s\n", integrated, stringValue(task, "target_branch"))
		}
	}
	if err != nil {
		return exitCode, err
	}
	if len(recordSlice(task, "attachment_failures")) > 0 {
		return 2, nil
	}
	status := stringValue(task, "status")
	if status == StatusCompleted || (status == StatusReady && !boolValue(task, "auto_integrate", true)) || stringValue(task, "integrated_commit") != "" {
		return 0, nil
	}
	if exitCode == 0 {
		exitCode = 2
	}
	return exitCode, nil
}

func openTaskCommand(store *Store, options launchOptions) (int, error) {
	if err := prepareLaunch(&options); err != nil {
		return 2, err
	}
	if options.Local {
		if options.New || options.RequireCurrent {
			return 2, fail("--local cannot be combined with --new or --require-current")
		}
		return launchNative(options.Agent, options.Description, options.Command, options.LaunchCWD, nil)
	}
	if codexSubcommand(options.Command) == "review" {
		options.RequireCurrent = true
	}
	command := options.Command
	if len(command) == 0 {
		var err error
		command, err = defaultAgentCommand(options.Agent, options.Description)
		if err != nil {
			return 2, err
		}
	}
	if err := validateForegroundAgentCommand(options.Agent, command, true); err != nil {
		return 2, err
	}
	checkout, err := repoRoot(options.LaunchCWD)
	if err != nil {
		return 2, err
	}
	repository, err := primaryWorktree(checkout)
	if err != nil {
		return 2, err
	}
	if options.RequireCurrent {
		if options.New {
			return 2, fail("--require-current cannot be combined with --new")
		}
		reservation, err := acquireCheckoutSession(store, checkout, true, sessionOptions{
			Agent: options.Agent, WorkingDirectory: options.LaunchCWD,
			Repository: repository,
		})
		if err != nil {
			return 2, err
		}
		if reservation == nil {
			return 2, fail("current checkout is busy; refusing to run this command against a different snapshot: %s", checkout)
		}
		memory, err := readNativeMemory(store, repository, checkout)
		if err != nil {
			reservation.Release(store, checkout, "")
			return 2, err
		}
		exitCode, launchErr := launchNative(options.Agent, options.Description, command, options.LaunchCWD, reservation)
		handoffTasks := reservation.CaptureHandoffTasks()
		reservation.Release(store, checkout, "")
		finalizeNativeMemory(store, memory)
		if launchErr != nil {
			return exitCode, launchErr
		}
		if exitCode == handoffExitCode && len(handoffTasks) > 0 {
			success := retryHandoffIntegrations(store, handoffTasks)
			retryReadyIntegrations(store, repository, nil)
			if success {
				return 0, nil
			}
			return 2, nil
		}
		retryReadyIntegrations(store, repository, nil)
		return exitCode, nil
	}

	tasks := refreshInterruptedTasks(store, repository)
	if !options.New && !options.Fresh {
		active := false
		for _, task := range tasks {
			if stringValue(task, "agent") == options.Agent && (stringValue(task, "status") == StatusCreated || stringValue(task, "status") == StatusRunning) && processAlive(task["process"]) {
				active = true
				break
			}
		}
		if !active {
			recoverable := []Record{}
			for _, task := range tasks {
				if stringValue(task, "agent") == options.Agent && stringValue(task, "status") == StatusRecovery {
					recoverable = append(recoverable, task)
				}
			}
			if len(recoverable) > 0 {
				selected, selectErr := chooseRecoveryTasks(recoverable)
				if selectErr != nil {
					return 2, selectErr
				}
				if len(selected) > 0 {
					return recoverSelected(store, selected, options)
				}
			}
		}
	}
	options.Command = command
	return startTaskCommand(store, options)
}

func recoveryTaskActivity(task Record) string {
	paths := recoveryChangedPaths(task)
	if len(paths) > 0 {
		return fmt.Sprintf("%d changed file(s)", len(paths))
	}
	head := currentHead(task)
	base := stringValue(task, "base_sha")
	if head != "" && base != "" && head != base {
		count, _ := gitCommand(stringValue(task, "repository"), false, "rev-list", "--count", base+".."+head)
		if value := strings.TrimSpace(count.Stdout); value != "" {
			return value + " commit(s)"
		}
	}
	return "preserved checkout"
}

func printRecoveryChoices(tasks []Record) {
	fmt.Println("Interrupted work found:")
	for index, task := range tasks {
		branch := firstNonempty(stringValue(task, "branch"), "no branch yet")
		target := firstNonempty(stringValue(task, "target_branch"), "no target")
		taskID := stringValue(task, "task_id")
		shortID := taskID[strings.LastIndex(taskID, "-")+1:]
		fmt.Printf("  %d. %.96s\n", index+1, recoveryTaskTitle(task))
		fmt.Printf("     %s -> %s | %s | id ...%s\n", branch, target, recoveryTaskActivity(task), shortID)
	}
}

func chooseRecoveryTasks(tasks []Record) ([]Record, error) {
	sortTasksNewest(tasks)
	if len(tasks) == 1 {
		return tasks, nil
	}
	printRecoveryChoices(tasks)
	info, _ := os.Stdin.Stat()
	if info == nil || info.Mode()&os.ModeCharDevice == 0 {
		choices := []string{}
		for _, task := range tasks {
			taskID := stringValue(task, "task_id")
			choices = append(choices, fmt.Sprintf("%s (...%s)", recoveryTaskTitle(task), taskID[strings.LastIndex(taskID, "-")+1:]))
		}
		return nil, fail("multiple interrupted tasks require a terminal selection: %s", strings.Join(choices, "; "))
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Resume: Enter=all, 1-N=one, n=new, q=cancel: ")
		answer, _ := reader.ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer == "" || answer == "a" || answer == "all" {
			return tasks, nil
		}
		if answer == "n" {
			return nil, nil
		}
		if answer == "q" {
			return nil, fail("cancelled")
		}
		if value, err := strconv.Atoi(answer); err == nil && value >= 1 && value <= len(tasks) {
			return []Record{tasks[value-1]}, nil
		}
	}
}

func recoverSelected(store *Store, tasks []Record, options launchOptions) (int, error) {
	for index, task := range tasks {
		taskID := stringValue(task, "task_id")
		shortID := taskID[strings.LastIndex(taskID, "-")+1:]
		position := ""
		if len(tasks) > 1 {
			position = fmt.Sprintf(" %d/%d", index+1, len(tasks))
		}
		fmt.Printf("Resuming%s: %s (...%s)\n", position, recoveryTaskTitle(task), shortID)
		var policy *bool
		if options.NoIntegrate {
			value := false
			policy = &value
		}
		result, err := recoverTask(store, taskID, options.Agent, policy, options.NewSession, options.Description, options.Command, options.Quiet)
		if err != nil || result != 0 {
			if remaining := len(tasks) - index - 1; remaining > 0 {
				fmt.Fprintf(os.Stderr, "Recovery queue paused; %d task(s) remain preserved.\n", remaining)
			}
			return result, err
		}
	}
	return 0, nil
}

func defaultChatResumeCommand(sessionID string, last, includeNonInteractive bool) ([]string, error) {
	if sessionID != "" && last {
		return nil, fail("choose a session id or --last, not both")
	}
	command := []string{"codex", "resume", "--dangerously-bypass-approvals-and-sandbox", "-c", `tui.resume_cwd="current"`}
	if sessionID != "" {
		command = append(command, sessionID)
	} else {
		command = append(command, "--all")
		if last {
			command = append(command, "--last")
		}
	}
	if includeNonInteractive {
		command = append(command, "--include-non-interactive")
	}
	return command, nil
}

func resumeArgumentsHaveSession(arguments []string) bool {
	for index := 0; index < len(arguments); {
		value := arguments[index]
		if value == "--" {
			return index+1 < len(arguments)
		}
		if value == "--last" || value == "--all" {
			return true
		}
		if codexGlobalValueOptions[value] {
			index += 2
			continue
		}
		if strings.HasPrefix(value, "-") {
			index++
			continue
		}
		return true
	}
	return false
}

func passthroughChatResumeCommand(arguments []string) []string {
	command := []string{"codex", "resume", "--dangerously-bypass-approvals-and-sandbox", "-c", `tui.resume_cwd="current"`}
	if !resumeArgumentsHaveSession(arguments) {
		command = append(command, "--all")
	}
	return append(command, arguments...)
}

type resumeOptions struct {
	SessionID             string
	Last                  bool
	All                   bool
	IncludeNonInteractive bool
	Target                string
	Checks                []string
	CheckTimeout          float64
	NoIntegrate           bool
	Quiet                 bool
	Arguments             []string
}

func resumeTaskCommand(store *Store, options resumeOptions) (int, error) {
	if len(options.Arguments) > 0 && (options.SessionID != "" || options.Last || options.All || options.IncludeNonInteractive) {
		return 2, fail("pass Codex resume options either before or after --, not both")
	}
	savedChatRequested := len(options.Arguments) > 0 || options.SessionID != "" || options.Last || options.All || options.IncludeNonInteractive
	command := passthroughChatResumeCommand(options.Arguments)
	var err error
	if len(options.Arguments) == 0 {
		command, err = defaultChatResumeCommand(options.SessionID, options.Last, options.IncludeNonInteractive)
		if err != nil {
			return 2, err
		}
	}
	launch := launchOptions{
		Description: "resume a saved Codex session", Task: "resume a saved Codex session",
		Agent: "codex", Target: options.Target, Checks: options.Checks,
		CheckTimeout: options.CheckTimeout, NoIntegrate: options.NoIntegrate,
		Quiet: options.Quiet, Command: command, LaunchCWD: currentDirectory(),
	}
	if err := prepareLaunch(&launch); err != nil {
		return 2, err
	}
	checkout, err := repoRoot(launch.LaunchCWD)
	if err != nil {
		return 2, err
	}
	repository, err := primaryWorktree(checkout)
	if err != nil {
		return 2, err
	}
	tasks := refreshInterruptedTasks(store, repository)
	if !savedChatRequested {
		recoverable := []Record{}
		for _, task := range tasks {
			if stringValue(task, "agent") == "codex" && stringValue(task, "status") == StatusRecovery {
				recoverable = append(recoverable, task)
			}
		}
		if len(recoverable) > 0 {
			selected, err := chooseRecoveryTasks(recoverable)
			if err != nil {
				return 2, err
			}
			if len(selected) > 0 {
				return recoverSelected(store, selected, launchOptions{Agent: "codex", Quiet: options.Quiet})
			}
		}
	}
	return startTaskCommand(store, launch)
}

func publishCommand(store *Store, selectedTaskID string) (int, error) {
	currentID := os.Getenv("MYRIAD_TASK_ID")
	currentWorktree := os.Getenv("MYRIAD_WORKTREE")
	if os.Getenv("MYRIAD_HARNESS") != "myriad" || currentID == "" || currentWorktree == "" {
		return 2, fail("publish must run from an active managed agent session")
	}
	current, err := store.Load(currentID)
	if err != nil {
		return 2, err
	}
	left, _ := canonical(currentWorktree)
	right, _ := canonical(stringValue(current, "worktree_path"))
	if left != right {
		return 2, fail("managed publish context does not match the active task")
	}
	if selectedTaskID == "" {
		selectedTaskID = currentID
	}
	task, err := store.Load(selectedTaskID)
	if err != nil {
		return 2, err
	}
	if selectedTaskID != currentID && stringValue(task, "attachment_parent_task_id") != currentID {
		return 2, fail("publish can target only the current task or one of its attachments")
	}
	result, err := publishTaskCheckpoint(store, task)
	if err != nil {
		return 2, err
	}
	fmt.Printf("%s: %s publish %.12s -> %s@%.12s\n", selectedTaskID, stringValue(result, "strategy"), stringValue(result, "result_commit"), firstNonempty(stringValue(task, "target_branch"), "target"), stringValue(result, "published_commit"))
	return 0, nil
}

func contextCommand(store *Store, taskID, jira, pr string, clearJira, clearPR bool) error {
	if taskID == "" {
		taskID = taskForWorkingDirectory(activeWorktreeTasks(store), currentDirectory())
	}
	if taskID == "" {
		taskID = os.Getenv("MYRIAD_TASK_ID")
	}
	if taskID == "" {
		return fail("no managed task; pass --task TASK_ID")
	}
	if _, err := store.Load(taskID); err != nil {
		return err
	}
	lock, err := store.Lock("context:"+taskID, true)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	context := readTaskContext(store, taskID)
	action := ""
	if clearJira {
		delete(context, "jira_issue")
		action = "Jira cleared"
	} else if clearPR {
		delete(context, "pull_request_number")
		action = "PR cleared"
	} else if jira != "" {
		issue, err := jiraIssue(jira)
		if err != nil {
			return err
		}
		context["jira_issue"] = issue
		action = "Jira " + issue
	} else {
		number, err := pullRequestNumber(pr)
		if err != nil {
			return err
		}
		context["pull_request_number"] = number
		action = fmt.Sprintf("PR #%d", number)
	}
	if err := writeTaskContext(store, taskID, context); err != nil {
		return err
	}
	fmt.Printf("context %s: %s\n", taskID, action)
	return nil
}

func inboxCommand(store *Store, sessionID, eventID string, asJSON bool) error {
	selected, _, _, err := currentAgentSession(store, sessionID)
	if err != nil {
		return err
	}
	lock, err := store.Lock("inbox:"+selected, true)
	if err != nil {
		return err
	}
	inbox, err := readSessionInbox(store, selected)
	_ = lock.Unlock()
	if err != nil {
		return err
	}
	messages := []Record{}
	for _, raw := range recordSlice(inbox, "messages") {
		message := anyRecord(raw)
		if eventID != "" && stringValue(message, "id") != eventID {
			continue
		}
		if eventID == "" && stringValue(message, "status") == "resolved" {
			continue
		}
		messages = append(messages, message)
	}
	if eventID != "" && len(messages) == 0 {
		return fail("unknown inbox event: %s", eventID)
	}
	if asJSON {
		payload, _ := json.MarshalIndent(Record{"session_id": selected, "messages": recordsToAny(messages)}, "", "  ")
		fmt.Println(string(payload))
		return nil
	}
	if len(messages) == 0 {
		fmt.Printf("%s: inbox empty\n", selected)
		return nil
	}
	for _, message := range messages {
		fmt.Printf("%s: %s (%s)\n  %s\n", stringValue(message, "id"), stringValue(message, "status"), firstNonempty(stringValue(message, "type"), "message"), stringValue(message, "prompt"))
	}
	return nil
}

func resolveObsoleteHandoff(store *Store, taskID string) bool {
	lock, err := store.Lock("task:"+taskID, false)
	if err != nil {
		return false
	}
	defer lock.Unlock()
	task, err := store.Load(taskID)
	if err != nil {
		return false
	}
	if stringValue(task, "status") == StatusIntegrated {
		resolveTaskNotices(store, taskID)
		return true
	}
	if stringValue(task, "status") != StatusReady {
		return false
	}
	return recognizeResultOnTarget(store, task)
}

func handoffCommand(store *Store, eventID string) error {
	sessionID, _, session, err := currentAgentSession(store, "")
	if err != nil {
		return err
	}
	if err := validateIdentifier(eventID, "inbox event id"); err != nil {
		return err
	}
	pending, err := pendingInboxMessages(store, sessionID, true)
	if err != nil {
		return err
	}
	var message Record
	for _, candidate := range pending {
		if stringValue(candidate, "id") == eventID {
			message = candidate
			break
		}
	}
	if message == nil {
		return fail("inbox event is not pending: %s", eventID)
	}
	taskID := stringValue(message, "task_id")
	if stringValue(message, "type") != "integration_ready" || taskID == "" {
		return fail("inbox event cannot trigger a repository handoff: %s", eventID)
	}
	if resolveObsoleteHandoff(store, taskID) {
		fmt.Printf("handoff no longer required: %s\ntask %s is already present on its target; this session remains open.\n", eventID, taskID)
		return nil
	}
	owner := recordMap(session, "process")
	if owner == nil || stringValue(owner, "role") != "lock-supervisor" || stringValue(session, "notification_state") != "ready" || !processAlive(owner) {
		return fail("session supervisor is no longer active")
	}
	if _, err := updateInboxEvent(store, sessionID, eventID, "accepted", "agent-command"); err != nil {
		return err
	}
	pid, _ := intValue(owner["pid"])
	if err := unix.Kill(pid, unix.SIGUSR2); err != nil {
		return fail("cannot notify the session supervisor: %v", err)
	}
	fmt.Printf("handoff accepted: %s\nMyriad will close this foreground session and retry integration after its lease is released.\n", eventID)
	return nil
}

func inboxHook() error {
	sessionID := os.Getenv(envAgentSessionID)
	if sessionID == "" {
		return nil
	}
	payload, err := io.ReadAll(io.LimitReader(os.Stdin, 65537))
	if err != nil || len(payload) > 65536 {
		return nil
	}
	value := Record{}
	if len(strings.TrimSpace(string(payload))) > 0 && decodeJSON(payload, &value) != nil {
		return nil
	}
	eventName := stringValue(value, "hook_event_name")
	if eventName != "Stop" && eventName != "UserPromptSubmit" {
		return nil
	}
	store, err := NewStore()
	if err != nil {
		return err
	}
	pending, err := pendingInboxMessages(store, sessionID, false)
	if err != nil || len(pending) == 0 {
		return err
	}
	prompts := []string{}
	for _, message := range pending {
		prompts = append(prompts, stringValue(message, "prompt"))
	}
	prompt := strings.Join(prompts, "\n\n")
	output := Record{}
	if eventName == "Stop" {
		output = Record{"decision": "block", "reason": prompt}
	} else {
		output = Record{"hookSpecificOutput": Record{"hookEventName": "UserPromptSubmit", "additionalContext": prompt}}
	}
	encoded, _ := json.Marshal(output)
	fmt.Println(string(encoded))
	for _, message := range pending {
		_, _ = updateInboxEvent(store, sessionID, stringValue(message, "id"), "delivered", "claude-"+eventName)
	}
	return nil
}

func cleanupCommand(store *Store, taskID string, all bool) int {
	tasks := store.All(true)
	if !all {
		task, err := store.Load(taskID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "myriad:", err)
			return 2
		}
		tasks = []Record{task}
	}
	failed := false
	for _, snapshot := range tasks {
		lock, err := store.Lock("task:"+stringValue(snapshot, "task_id"), false)
		if err != nil {
			fmt.Printf("%s: busy; skipped\n", stringValue(snapshot, "task_id"))
			failed = true
			continue
		}
		task, _ := store.Load(stringValue(snapshot, "task_id"))
		if processAlive(task["process"]) {
			fmt.Printf("%s: active; skipped\n", stringValue(task, "task_id"))
			failed = true
			_ = lock.Unlock()
			continue
		}
		cleaned, _ := cleanupTask(store, task, false, false)
		_ = lock.Unlock()
		if cleaned {
			fmt.Printf("%s: cleaned\n", stringValue(task, "task_id"))
		} else {
			fmt.Printf("%s: preserved\n", stringValue(task, "task_id"))
			failed = true
		}
	}
	if failed {
		return 2
	}
	return 0
}
