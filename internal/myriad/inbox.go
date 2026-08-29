package myriad

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func emptyInbox(sessionID string) (Record, error) {
	if err := validateIdentifier(sessionID, "session id"); err != nil {
		return nil, err
	}
	return Record{
		"schema_version": InboxSchema,
		"session_id":     sessionID,
		"messages":       []any{},
		"updated_at":     now(),
	}, nil
}

func validateInbox(value Record, expectedSessionID string) error {
	schema, ok := intValue(value["schema_version"])
	if !ok || schema != InboxSchema {
		return fail("unsupported session inbox schema: %s", describe(value["schema_version"]))
	}
	if stringValue(value, "session_id") != expectedSessionID {
		return fail("session inbox id mismatch: expected %q, found %q", expectedSessionID, stringValue(value, "session_id"))
	}
	messages, ok := value["messages"].([]any)
	if !ok {
		return fail("session inbox messages must be a JSON array")
	}
	for _, raw := range messages {
		message, ok := toAnyMap(raw)
		if !ok {
			return fail("session inbox message must be a JSON object")
		}
		eventID, _ := message["id"].(string)
		if err := validateIdentifier(eventID, "inbox event id"); err != nil {
			return err
		}
		status, _ := message["status"].(string)
		if status != "pending" && status != "delivered" && status != "accepted" && status != "resolved" {
			return fail("invalid inbox event status: %s", describe(message["status"]))
		}
		prompt, _ := message["prompt"].(string)
		if strings.TrimSpace(prompt) == "" {
			return fail("session inbox message has no prompt")
		}
	}
	return nil
}

func readSessionInbox(store *Store, sessionID string) (Record, error) {
	path, err := store.InboxPath(sessionID)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return emptyInbox(sessionID)
	}
	var value Record
	if err := readJSON(path, maxInboxBytes, &value); err != nil {
		return nil, fail("cannot safely read session inbox %s: %v", path, err)
	}
	if err := validateInbox(value, sessionID); err != nil {
		return nil, err
	}
	return value, nil
}

func writeSessionInbox(store *Store, inbox Record) error {
	sessionID := stringValue(inbox, "session_id")
	if sessionID == "" {
		return fail("session inbox has no session id")
	}
	if err := validateInbox(inbox, sessionID); err != nil {
		return err
	}
	inbox["updated_at"] = now()
	payload, err := marshalPrivate(inbox)
	if err != nil {
		return err
	}
	if len(payload) > maxInboxBytes {
		return fail("session inbox is too large: %s", sessionID)
	}
	path, _ := store.InboxPath(sessionID)
	return atomicWrite(path, payload, 0o600)
}

func inboxEventID(sessionID, taskID string) string {
	digest := sha256Hex([]byte("integration-ready\x00" + sessionID + "\x00" + taskID))
	return "ready-" + digest[:24]
}

func integrationNoticePrompt(task Record, eventID string) string {
	return fmt.Sprintf(
		"myriad event %s: managed task %s is complete and waiting for integration, but this session currently owns a repository lease. Finish the smallest safe checkpoint for your current work. Do not merge, cherry-pick, switch branches, or clean worktrees manually. Then run `myriad handoff %s` so Myriad can release this session, finalize managed work, and retry the queued integration automatically. Use `myriad inbox` to inspect the durable event before handing off.",
		eventID, stringValue(task, "task_id"), eventID,
	)
}

func enqueueIntegrationNotice(store *Store, session, task Record) (string, error) {
	sessionID := stringValue(session, "session_id")
	taskID := stringValue(task, "task_id")
	if sessionID == "" || taskID == "" {
		return "", fail("integration notice has incomplete identity")
	}
	eventID := inboxEventID(sessionID, taskID)
	lock, err := store.Lock("inbox:"+sessionID, true)
	if err != nil {
		return "", err
	}
	defer lock.Unlock()
	inbox, err := readSessionInbox(store, sessionID)
	if err != nil {
		return "", err
	}
	messages := recordSlice(inbox, "messages")
	found := false
	for _, raw := range messages {
		message, ok := toAnyMap(raw)
		if !ok || message["id"] != eventID {
			continue
		}
		found = true
		if message["status"] == "resolved" {
			message["status"] = "pending"
			message["prompt"] = integrationNoticePrompt(task, eventID)
			message["requeued_at"] = now()
			delete(message, "resolved_at")
		}
	}
	if !found {
		messages = append(messages, Record{
			"id":            eventID,
			"type":          "integration_ready",
			"status":        "pending",
			"task_id":       taskID,
			"target_branch": task["target_branch"],
			"repository":    task["repository"],
			"prompt":        integrationNoticePrompt(task, eventID),
			"created_at":    now(),
		})
		inbox["messages"] = messages
	}
	return eventID, writeSessionInbox(store, inbox)
}

func pendingInboxMessages(store *Store, sessionID string, includeDelivered bool) ([]Record, error) {
	lock, err := store.Lock("inbox:"+sessionID, true)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	inbox, err := readSessionInbox(store, sessionID)
	if err != nil {
		return nil, err
	}
	result := []Record{}
	for _, raw := range recordSlice(inbox, "messages") {
		message, ok := toAnyMap(raw)
		if !ok {
			continue
		}
		status, _ := message["status"].(string)
		if status == "pending" || (includeDelivered && status == "delivered") {
			result = append(result, cloneRecord(message))
		}
	}
	return result, nil
}

func updateInboxEvent(store *Store, sessionID, eventID, status, detail string) (Record, error) {
	allowed := map[string]map[string]bool{
		"delivered": {"pending": true, "delivered": true},
		"accepted":  {"pending": true, "delivered": true, "accepted": true},
		"resolved":  {"pending": true, "delivered": true, "accepted": true, "resolved": true},
	}
	transitions, ok := allowed[status]
	if !ok {
		return nil, fail("unsupported inbox event transition: %s", status)
	}
	lock, err := store.Lock("inbox:"+sessionID, true)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	inbox, err := readSessionInbox(store, sessionID)
	if err != nil {
		return nil, err
	}
	var selected Record
	for _, raw := range recordSlice(inbox, "messages") {
		message, ok := toAnyMap(raw)
		if !ok || message["id"] != eventID {
			continue
		}
		current, _ := message["status"].(string)
		if !transitions[current] {
			return nil, fail("inbox event %s cannot move from %s to %s", eventID, current, status)
		}
		message["status"] = status
		message[status+"_at"] = now()
		if detail != "" {
			message[status+"_via"] = detail
		}
		selected = cloneRecord(message)
		break
	}
	if selected == nil {
		return nil, fail("unknown inbox event: %s", eventID)
	}
	return selected, writeSessionInbox(store, inbox)
}

func resolveTaskNotices(store *Store, taskID string) {
	entries, _ := os.ReadDir(store.Inboxes)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		sessionID := strings.TrimSuffix(entry.Name(), ".json")
		lock, err := store.Lock("inbox:"+sessionID, true)
		if err != nil {
			continue
		}
		inbox, err := readSessionInbox(store, sessionID)
		if err == nil {
			changed := false
			for _, raw := range recordSlice(inbox, "messages") {
				message, ok := toAnyMap(raw)
				if ok && message["task_id"] == taskID && message["status"] != "resolved" {
					message["status"] = "resolved"
					message["resolved_at"] = now()
					changed = true
				}
			}
			if changed {
				_ = writeSessionInbox(store, inbox)
			}
		}
		_ = lock.Unlock()
	}
}

func acceptedHandoffTasks(store *Store, sessionID string) ([]string, error) {
	lock, err := store.Lock("inbox:"+sessionID, true)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	inbox, err := readSessionInbox(store, sessionID)
	if err != nil {
		return nil, err
	}
	authorized := false
	for _, raw := range recordSlice(inbox, "messages") {
		message, ok := toAnyMap(raw)
		if ok && message["status"] == "accepted" {
			authorized = true
			break
		}
	}
	changed := false
	if authorized {
		for _, raw := range recordSlice(inbox, "messages") {
			message, ok := toAnyMap(raw)
			if !ok || message["type"] != "integration_ready" {
				continue
			}
			if message["status"] == "pending" || message["status"] == "delivered" {
				message["status"] = "accepted"
				message["accepted_at"] = now()
				message["accepted_via"] = "session-handoff"
				changed = true
			}
		}
	}
	if changed {
		if err := writeSessionInbox(store, inbox); err != nil {
			return nil, err
		}
	}
	result := []string{}
	for _, raw := range recordSlice(inbox, "messages") {
		message, ok := toAnyMap(raw)
		if ok && message["status"] == "accepted" {
			if taskID, ok := message["task_id"].(string); ok {
				result = append(result, taskID)
			}
		}
	}
	sort.Strings(result)
	return result, nil
}
