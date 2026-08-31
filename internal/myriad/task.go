package myriad

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	taskSlugPattern  = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	jiraIssuePattern = regexp.MustCompile(`([A-Z][A-Z0-9_]{1,31}-[1-9][0-9]*)`)
	jiraExactPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,31}-[1-9][0-9]*$`)
	prTextPattern    = regexp.MustCompile(`(?i)(?:https://github\.com/[^/\s]+/[^/\s]+/pull/|\b(?:pr|pull[ _-]*request)\s*#?\s*)([1-9][0-9]*)\b`)
)

func taskSlug(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > codexSlugLimit || !taskSlugPattern.MatchString(value) {
		return "", fail("invalid task slug: %q", value)
	}
	return value, nil
}

func fallbackTaskSlug(value string) string {
	wordsPattern := regexp.MustCompile(`[a-z0-9]+`)
	ignored := map[string]struct{}{
		"a": {}, "an": {}, "and": {}, "for": {}, "in": {}, "of": {},
		"on": {}, "the": {}, "to": {}, "with": {},
	}
	selected := []string{}
	for _, word := range wordsPattern.FindAllString(strings.ToLower(value), -1) {
		if _, skip := ignored[word]; skip || len(word) > codexSlugLimit {
			continue
		}
		candidate := strings.Join(append(append([]string{}, selected...), word), "-")
		if len(candidate) <= codexSlugLimit {
			selected = append(selected, word)
		}
		if len(selected) == 3 {
			break
		}
	}
	if len(selected) == 0 {
		return "task"
	}
	return strings.Join(selected, "-")
}

func storedTaskTitle(task Record) string {
	for _, key := range []string{"title", "provisioning_slug"} {
		if value := strings.Join(strings.Fields(stringValue(task, key)), " "); value != "" {
			return value
		}
	}
	description := strings.Join(strings.Fields(stringValue(task, "description")), " ")
	if description != "" {
		if _, generic := genericTaskDescriptions[strings.ToLower(description)]; !generic {
			return description
		}
	}
	return stringValue(task, "branch")
}

func taskWorktreeState(task Record) string {
	state := stringValue(task, "worktree_state")
	if state == "" {
		return worktreePending
	}
	return state
}

func taskWorktreeReady(task Record) bool {
	return taskWorktreeState(task) == worktreeReady
}

func taskConfiguredWorkingDirectory(task Record) string {
	worktree, _ := canonical(stringValue(task, "worktree_path"))
	relative := stringValue(task, "workdir_relative")
	if relative == "" {
		relative = "."
	}
	working, _ := canonical(filepath.Join(worktree, relative))
	if !isWithin(working, worktree) {
		return worktree
	}
	return working
}

func taskWorkingDirectory(task Record) string {
	configured := taskConfiguredWorkingDirectory(task)
	if info, err := os.Stat(configured); err == nil && info.IsDir() {
		return configured
	}
	return stringValue(task, "worktree_path")
}

func taskOriginWorkingDirectory(task Record) (string, error) {
	value := stringValue(task, "origin_working_directory")
	if info, err := os.Stat(value); err == nil && info.IsDir() {
		return canonical(value)
	}
	return "", fail("managed Codex task has no valid origin working directory")
}

func managedWorktreePath(store *Store, task Record) (string, error) {
	value := stringValue(task, "worktree_path")
	if value == "" {
		return "", fail("task has no managed worktree path")
	}
	path, _ := canonical(value)
	root, _ := canonical(store.Worktrees)
	if !isWithin(path, root) || filepath.Base(path) != stringValue(task, "task_id") || filepath.Dir(filepath.Dir(path)) != root {
		return "", fail("refused unexpected managed worktree path: %s", path)
	}
	return path, nil
}

func taskCheckoutIdentity(task Record) (string, error) {
	common := stringValue(task, "git_common_dir")
	worktree := stringValue(task, "worktree_path")
	if common == "" || worktree == "" {
		return "", fail("task has incomplete checkout identity")
	}
	resolvedCommon, _ := canonical(common)
	resolvedWorktree, _ := canonical(worktree)
	return resolvedCommon + "\x00" + resolvedWorktree, nil
}

func taskEnvironment(task Record) []string {
	environment := environmentWithout(os.Environ(),
		envAgentSessionPath, envAgentSessionID,
	)
	values := map[string]string{
		"MYRIAD_HARNESS":       "myriad",
		"MYRIAD_TASK_ID":       stringValue(task, "task_id"),
		"MYRIAD_TASK_TITLE":    storedTaskTitle(task),
		"MYRIAD_WORKTREE":      stringValue(task, "worktree_path"),
		"MYRIAD_WORKDIR":       taskConfiguredWorkingDirectory(task),
		"MYRIAD_TARGET_BRANCH": stringValue(task, "target_branch"),
		"MYRIAD_REPO_MEMORY":   MemoryName,
		"MYRIAD_MEMORY_SOURCE": stringValue(task, "memory_path"),
	}
	if branch := stringValue(task, "branch"); branch != "" {
		values["MYRIAD_BRANCH"] = branch
	}
	return overlayEnvironment(environment, values)
}

func environmentWithout(environment []string, names ...string) []string {
	blocked := map[string]struct{}{}
	for _, name := range names {
		blocked[name] = struct{}{}
	}
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if _, remove := blocked[name]; !remove {
			result = append(result, entry)
		}
	}
	return result
}

func overlayEnvironment(environment []string, values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	result := environmentWithout(environment, names...)
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	return result
}

func nativeAgentEnvironment() []string {
	return environmentWithout(os.Environ(),
		"MYRIAD_POLICY", envInheritedLockFDs, envLockSessionPath,
		envLockSessionID, envAgentSessionPath, envAgentSessionID,
		"MYRIAD_HARNESS", "MYRIAD_TASK_ID", "MYRIAD_TASK_TITLE",
		"MYRIAD_WORKTREE", "MYRIAD_WORKDIR", "MYRIAD_TARGET_BRANCH",
		"MYRIAD_BRANCH", "MYRIAD_REPO_MEMORY", "MYRIAD_MEMORY_SOURCE",
	)
}

func jiraIssue(value string) (string, error) {
	issue := strings.ToUpper(strings.TrimSpace(value))
	if !jiraExactPattern.MatchString(issue) {
		return "", fail("invalid Jira issue key: %q", value)
	}
	return issue, nil
}

func jiraIssuesFromTask(task Record) []string {
	issues := []string{}
	seen := map[string]bool{}
	for _, field := range []string{"description", "source_branch"} {
		for _, match := range jiraIssuePattern.FindAllStringSubmatch(stringValue(task, field), -1) {
			if len(match) > 1 && !seen[match[1]] {
				issues = append(issues, match[1])
				seen[match[1]] = true
			}
		}
	}
	return issues
}

func pullRequestNumber(value string) (int, error) {
	text := strings.TrimSpace(value)
	text = strings.TrimPrefix(text, "#")
	if parsed, err := strconv.Atoi(text); err == nil && parsed > 0 {
		return parsed, nil
	}
	if match := prTextPattern.FindStringSubmatch(value); len(match) > 1 {
		parsed, err := strconv.Atoi(match[1])
		if err == nil && parsed > 0 {
			return parsed, nil
		}
	}
	return 0, fail("invalid pull request number: %q", value)
}

func pullRequestsFromTask(task Record) []int {
	numbers := []int{}
	seen := map[int]bool{}
	for _, match := range prTextPattern.FindAllStringSubmatch(stringValue(task, "description"), -1) {
		if len(match) <= 1 {
			continue
		}
		value, _ := strconv.Atoi(match[1])
		if value > 0 && !seen[value] {
			numbers = append(numbers, value)
			seen[value] = true
		}
	}
	return numbers
}

func setStatus(store *Store, task Record, status, reason string) error {
	task["status"] = status
	if reason == "" {
		delete(task, "status_reason")
	} else {
		task["status_reason"] = reason
	}
	return store.Save(task)
}

func currentHead(task Record) string {
	worktree := stringValue(task, "worktree_path")
	if info, err := os.Stat(worktree); err == nil && info.IsDir() {
		if value, err := gitRef(worktree, "HEAD"); err == nil {
			return value
		}
	}
	repository := stringValue(task, "repository")
	branch := stringValue(task, "branch")
	if branch != "" && branchExists(repository, branch) {
		value, _ := gitRef(repository, "refs/heads/"+branch)
		return value
	}
	return stringValue(task, "result_commit")
}

func formatTaskID() (string, error) {
	random, err := randomHex(3)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s", timeNow().Format("20060102-150405"), random), nil
}

var timeNow = func() time.Time { return time.Now() }
