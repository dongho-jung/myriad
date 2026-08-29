package myriad

import (
	"errors"
	"os"
)

func readTaskContext(store *Store, taskID string) Record {
	path, err := store.ContextPath(taskID)
	if err != nil {
		return Record{}
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return Record{}
	}
	var value Record
	if err := readJSON(path, maxJSONBytes, &value); err != nil {
		return Record{}
	}
	schema, ok := intValue(value["schema_version"])
	if !ok || schema != ContextSchema || stringValue(value, "task_id") != taskID {
		return Record{}
	}
	if issue := stringValue(value, "jira_issue"); issue != "" {
		if normalized, err := jiraIssue(issue); err == nil {
			value["jira_issue"] = normalized
		} else {
			return Record{}
		}
	}
	if raw, exists := value["pull_request_number"]; exists {
		number, ok := intValue(raw)
		if !ok || number <= 0 {
			return Record{}
		}
		value["pull_request_number"] = number
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
	if issue := stringValue(context, "jira_issue"); issue != "" {
		normalized, err := jiraIssue(issue)
		if err != nil {
			return err
		}
		context["jira_issue"] = normalized
	}
	payload, err := marshalPrivate(context)
	if err != nil {
		return err
	}
	path, _ := store.ContextPath(taskID)
	return atomicWrite(path, payload, 0o600)
}
