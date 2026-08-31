package myriad

import (
	"errors"
	"os"
)

func appendUniqueStrings(values []string, additions ...string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		seen[value] = true
	}
	for _, value := range additions {
		if value != "" && !seen[value] {
			values = append(values, value)
			seen[value] = true
		}
	}
	return values
}

func appendUniqueInts(values []int, additions ...int) []int {
	seen := map[int]bool{}
	for _, value := range values {
		seen[value] = true
	}
	for _, value := range additions {
		if value > 0 && !seen[value] {
			values = append(values, value)
			seen[value] = true
		}
	}
	return values
}

func normalizedJiraIssues(value any) ([]string, error) {
	raw := []string{}
	switch current := value.(type) {
	case nil:
	case string:
		raw = append(raw, current)
	case []string:
		raw = append(raw, current...)
	case []any:
		for _, item := range current {
			issue, ok := item.(string)
			if !ok {
				return nil, fail("invalid Jira issue list")
			}
			raw = append(raw, issue)
		}
	default:
		return nil, fail("invalid Jira issue list")
	}

	result := []string{}
	for _, value := range raw {
		issue, err := jiraIssue(value)
		if err != nil {
			return nil, err
		}
		result = appendUniqueStrings(result, issue)
	}
	return result, nil
}

func normalizedPullRequestNumbers(value any) ([]int, error) {
	raw := []any{}
	switch current := value.(type) {
	case nil:
	case int:
		raw = append(raw, current)
	case []int:
		for _, number := range current {
			raw = append(raw, number)
		}
	case []any:
		raw = append(raw, current...)
	default:
		raw = append(raw, current)
	}

	result := []int{}
	for _, value := range raw {
		number, ok := intValue(value)
		if !ok || number <= 0 {
			return nil, fail("invalid pull request number list")
		}
		result = appendUniqueInts(result, number)
	}
	return result, nil
}

func taskContextJiraIssues(context Record) ([]string, bool, error) {
	value, exists := context["jira_issues"]
	if !exists {
		value, exists = context["jira_issue"]
	}
	if !exists {
		return nil, false, nil
	}
	issues, err := normalizedJiraIssues(value)
	return issues, true, err
}

func taskContextPullRequestNumbers(context Record) ([]int, bool, error) {
	value, exists := context["pull_request_numbers"]
	if !exists {
		value, exists = context["pull_request_number"]
	}
	if !exists {
		return nil, false, nil
	}
	numbers, err := normalizedPullRequestNumbers(value)
	return numbers, true, err
}

func normalizeTaskContext(context Record) error {
	issues, hasIssues, err := taskContextJiraIssues(context)
	if err != nil {
		return err
	}
	if hasIssues {
		context["jira_issues"] = issues
	}
	delete(context, "jira_issue")

	numbers, hasNumbers, err := taskContextPullRequestNumbers(context)
	if err != nil {
		return err
	}
	if hasNumbers {
		context["pull_request_numbers"] = numbers
	}
	delete(context, "pull_request_number")
	return nil
}

func loadTaskContext(store *Store, taskID string) (Record, error) {
	path, err := store.ContextPath(taskID)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, nil
		}
		return nil, err
	}
	var value Record
	if err := readJSON(path, maxJSONBytes, &value); err != nil {
		return nil, fail("cannot safely read task context %s: %v", path, err)
	}
	schema, ok := intValue(value["schema_version"])
	if !ok || schema != ContextSchema || stringValue(value, "task_id") != taskID {
		return nil, fail("invalid task context: %s", path)
	}
	if err := normalizeTaskContext(value); err != nil {
		return nil, err
	}
	return value, nil
}

func readTaskContext(store *Store, taskID string) Record {
	value, err := loadTaskContext(store, taskID)
	if err != nil {
		return Record{}
	}
	return value
}

func writeTaskContext(store *Store, taskID string, context Record) error {
	if err := validateIdentifier(taskID, "task id"); err != nil {
		return err
	}
	context["schema_version"] = ContextSchema
	context["task_id"] = taskID
	context["updated_at"] = now()
	if err := normalizeTaskContext(context); err != nil {
		return err
	}
	payload, err := marshalPrivate(context)
	if err != nil {
		return err
	}
	if len(payload) > maxJSONBytes {
		return fail("task context is too large: %s", taskID)
	}
	path, _ := store.ContextPath(taskID)
	return atomicWrite(path, payload, 0o600)
}
