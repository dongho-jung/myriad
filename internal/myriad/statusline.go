package myriad

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func activeWorktreeTasks(store *Store) []Record {
	result := []Record{}
	for _, task := range store.All(false) {
		status := stringValue(task, "status")
		if (status != StatusCreated && status != StatusRunning) || stringValue(task, "worktree_path") == "" || !processAlive(task["process"]) {
			continue
		}
		item := cloneRecord(task)
		issues, numbers := taskDisplayContext(store, item)
		if len(issues) > 0 {
			item["statusline_jira_issues"] = issues
		}
		if len(numbers) > 0 {
			item["statusline_pull_request_numbers"] = numbers
		}
		result = append(result, item)
	}
	return result
}

func taskDisplayContext(store *Store, task Record) ([]string, []int) {
	context := readTaskContext(store, stringValue(task, "task_id"))
	issues, hasIssues, _ := taskContextJiraIssues(context)
	if !hasIssues {
		issues = jiraIssuesFromTask(task)
	}
	numbers, hasNumbers, _ := taskContextPullRequestNumbers(context)
	if !hasNumbers {
		numbers = pullRequestsFromTask(task)
	}
	return issues, numbers
}

func displayContext(issues []string, pullRequests []int) string {
	groups := []string{}
	if len(issues) > 0 {
		labels := make([]string, 0, len(issues))
		for _, issue := range issues {
			labels = append(labels, compactASCII(issue, "Jira", 40))
		}
		groups = append(groups, "["+strings.Join(labels, " ")+"]")
	}
	if len(pullRequests) > 0 {
		labels := make([]string, 0, len(pullRequests))
		for _, pullRequest := range pullRequests {
			labels = append(labels, fmt.Sprintf("#%d", pullRequest))
		}
		groups = append(groups, "["+strings.Join(labels, " ")+"]")
	}
	return strings.Join(groups, " ")
}

func codexTaskStatusTitle(store *Store, taskID string) string {
	selected, err := store.Load(taskID)
	if err != nil {
		return ""
	}
	primary := selected
	if parentID := stringValue(selected, "attachment_parent_task_id"); parentID != "" {
		if parent, loadErr := store.Load(parentID); loadErr == nil {
			primary = parent
		}
	}

	candidates := []Record{selected}
	seen := map[string]bool{stringValue(selected, "task_id"): true}
	if primaryID := stringValue(primary, "task_id"); primaryID != "" && !seen[primaryID] {
		candidates = append(candidates, primary)
		seen[primaryID] = true
	}
	attachments := []Record{}
	for _, task := range store.All(false) {
		if stringValue(task, "attachment_parent_task_id") != stringValue(primary, "task_id") || seen[stringValue(task, "task_id")] {
			continue
		}
		status := stringValue(task, "status")
		if status != StatusCreated && status != StatusRunning {
			continue
		}
		attachments = append(attachments, task)
	}
	sort.SliceStable(attachments, func(i, j int) bool {
		return stringValue(attachments[i], "created_at") < stringValue(attachments[j], "created_at")
	})
	candidates = append(candidates, attachments...)

	issues := []string{}
	for _, task := range candidates {
		candidateIssues, _ := taskDisplayContext(store, task)
		issues = appendUniqueStrings(issues, candidateIssues...)
	}

	branch := firstNonempty(stringValue(selected, "branch"), storedTaskTitle(selected), "task")
	if target := stringValue(selected, "target_branch"); target != "" && target != branch {
		branch += " -> " + target
	}
	context := displayContext(issues, nil)
	if context == "" {
		return branch
	}
	return context + " " + branch
}

func taskForWorkingDirectory(tasks []Record, current string) string {
	if current == "" {
		return ""
	}
	resolved, err := canonical(current)
	if err != nil {
		return ""
	}
	bestID := ""
	bestDepth := 0
	for _, task := range tasks {
		worktree := stringValue(task, "worktree_path")
		if worktree == "" {
			continue
		}
		worktree, _ = canonical(worktree)
		if (resolved == worktree || isWithin(resolved, worktree)) && len(strings.Split(worktree, string(filepath.Separator))) > bestDepth {
			bestDepth = len(strings.Split(worktree, string(filepath.Separator)))
			bestID = stringValue(task, "task_id")
		}
	}
	return bestID
}

func compactASCII(value, fallback string, limit int) string {
	text := strings.Join(strings.Fields(value), " ")
	builder := strings.Builder{}
	for _, character := range text {
		if character >= 32 && character < 127 {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('?')
		}
	}
	result := builder.String()
	if result == "" {
		result = fallback
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func taskLabel(task Record, current bool) string {
	repository := compactASCII(filepath.Base(stringValue(task, "repository")), "repo", 28)
	agent := compactASCII(stringValue(task, "agent"), "agent", 8)
	stamp := ""
	pattern := regexp.MustCompile(`^\d{8}-(\d{2})(\d{2})\d{2}-[A-Za-z0-9]+$`)
	if match := pattern.FindStringSubmatch(stringValue(task, "task_id")); len(match) > 2 {
		stamp = "@" + match[1] + ":" + match[2]
	}
	attachment := ""
	if stringValue(task, "attachment_parent_task_id") != "" {
		attachment = "+"
	}
	issues, _ := normalizedJiraIssues(task["statusline_jira_issues"])
	pullRequests, _ := normalizedPullRequestNumbers(task["statusline_pull_request_numbers"])
	context := displayContext(issues, pullRequests)
	marker := ""
	if current {
		marker = "*"
	}
	return marker + agent + "/" + repository + stamp + attachment + context
}

func claudeWorktreeLabel(payload Record) string {
	workspace := recordMap(payload, "workspace")
	if workspace == nil {
		return ""
	}
	worktree := stringValue(workspace, "git_worktree")
	if strings.TrimSpace(worktree) == "" {
		return ""
	}
	repositoryName := ""
	if repository := recordMap(workspace, "repo"); repository != nil {
		repositoryName = stringValue(repository, "name")
	}
	if repositoryName == "" {
		project := stringValue(workspace, "project_dir")
		if project == "" {
			project = stringValue(workspace, "current_dir")
		}
		if project == "" {
			project = stringValue(payload, "cwd")
		}
		repositoryName = filepath.Base(project)
	}
	return "*claude/" + compactASCII(repositoryName, "repo", 28) + "@" + compactASCII(worktree, "worktree", 24)
}

func renderStatusline(tasks []Record, currentTaskID string, width int, epoch float64, extra string) string {
	sort.SliceStable(tasks, func(i, j int) bool {
		return stringValue(tasks[i], "created_at") > stringValue(tasks[j], "created_at")
	})
	currentEntries := []string{}
	otherEntries := []string{}
	for _, task := range tasks {
		current := stringValue(task, "task_id") == currentTaskID
		if current {
			currentEntries = append(currentEntries, taskLabel(task, true))
		} else {
			otherEntries = append(otherEntries, taskLabel(task, false))
		}
	}
	if extra != "" {
		currentEntries = append([]string{extra}, currentEntries...)
	}
	count := len(currentEntries) + len(otherEntries)
	if count == 0 {
		return ""
	}
	if width < 12 {
		width = 12
	}
	if width > 1000 {
		width = 1000
	}
	prefix := fmt.Sprintf("WT %d | ", count)
	if len(currentEntries) > 0 {
		prefix += strings.Join(currentEntries, " | ")
		if len(otherEntries) > 0 {
			prefix += " | "
		}
	}
	body := strings.Join(otherEntries, " | ")
	complete := prefix + body
	if len(complete) <= width {
		return complete
	}
	if len(otherEntries) == 0 || len(prefix) >= width {
		if len(prefix) > width {
			return prefix[:width]
		}
		return prefix
	}
	track := strings.Join(otherEntries, " · ") + "   "
	offset := int(math.Floor(epoch)) % len(track)
	doubled := track + track
	needed := width - len(prefix)
	for len(doubled) < offset+needed {
		doubled += track
	}
	return prefix + doubled[offset:offset+needed]
}

func statusline(store *Store, payload Record, claude bool, width int, epoch float64) string {
	currentDirectory := ""
	if workspace := recordMap(payload, "workspace"); workspace != nil {
		currentDirectory = stringValue(workspace, "current_dir")
	}
	if currentDirectory == "" {
		currentDirectory = stringValue(payload, "cwd")
	}
	tasks := activeWorktreeTasks(store)
	current := taskForWorkingDirectory(tasks, currentDirectory)
	if current == "" {
		current = os.Getenv("MYRIAD_TASK_ID")
	}
	known := false
	for _, task := range tasks {
		if stringValue(task, "task_id") == current {
			known = true
			break
		}
	}
	extra := ""
	if claude && !known {
		extra = claudeWorktreeLabel(payload)
	}
	if epoch == 0 {
		epoch = float64(time.Now().Unix())
	}
	return renderStatusline(tasks, current, width, epoch, extra)
}
