package myriad

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Claude merges CLI settings with its normal settings layers. Fold any
// caller-supplied --settings into the same object so their hooks survive too.
func claudeActivityCommand(command []string, launcher string) ([]string, error) {
	executable := commandExecutableIndex(command, "claude")
	if executable < 0 {
		return command, nil
	}
	settings := Record{}
	result := append([]string{}, command[:executable+1]...)
	arguments := command[executable+1:]
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			result = append(result, arguments[index:]...)
			break
		}
		value, inline := strings.CutPrefix(argument, "--settings=")
		if argument != "--settings" && !inline {
			result = append(result, argument)
			continue
		}
		if !inline {
			index++
			if index >= len(arguments) {
				return nil, fail("--settings requires a value")
			}
			value = arguments[index]
		}
		payload := []byte(value)
		if !strings.HasPrefix(strings.TrimSpace(value), "{") {
			var err error
			payload, err = readClaudeActivitySettings(value)
			if err != nil {
				return nil, fmt.Errorf("cannot read Claude settings: %w", err)
			}
		}
		if err := decodeJSON(payload, &settings); err != nil || settings == nil {
			return nil, fail("Claude settings must be a JSON object")
		}
	}
	hooks := recordMap(settings, "hooks")
	if hooks == nil {
		if settings["hooks"] != nil {
			return nil, fail("Claude hooks settings must be a JSON object")
		}
		hooks = Record{}
	}
	for _, event := range []string{"UserPromptSubmit", "PostToolUse", "Stop"} {
		existing := recordSlice(hooks, event)
		if hooks[event] != nil && existing == nil {
			return nil, fail("Claude %s hooks must be a JSON array", event)
		}
		hooks[event] = append(existing, Record{"hooks": []any{Record{
			"type": "command", "command": hookCommand(internalActivityHook, launcher), "timeout": 10,
		}}})
	}
	settings["hooks"] = hooks
	encoded, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	// Insert options before any positional prompt or option terminator.
	result = append(result[:executable+1], append([]string{"--settings", string(encoded)}, result[executable+1:]...)...)
	return result, nil
}

func readClaudeActivitySettings(path string) ([]byte, error) {
	// Caller-selected settings can be symlinked or owned by an administrator;
	// they are not private Myriad state files.
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fail("Claude settings must be a regular file")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maxJSONBytes+1))
	if int64(len(payload)) > maxJSONBytes {
		return nil, fail("Claude settings are too large")
	}
	return payload, err
}

func activityHookContext(payload Record, deliver func(string) error) error {
	if os.Getenv("MYRIAD_HARNESS") != "myriad" || os.Getenv(envAgentSessionID) == "" || stringValue(payload, "agent_id") != "" {
		return nil
	}
	event := stringValue(payload, "hook_event_name")
	if event != "UserPromptSubmit" && event != "PostToolUse" && event != "Stop" {
		return nil
	}
	store, err := NewStore()
	if err != nil {
		return err
	}
	sessionID, sessionPath, session, err := currentAgentSession(store, "")
	if err != nil {
		return err
	}
	if stringValue(session, "task_id") == "" {
		return nil
	}
	if stringValue(session, "agent") == "codex" {
		threadID := stringValue(session, "codex_thread_id")
		if threadID != "" && stringValue(payload, "session_id") != threadID {
			return nil
		}
	}
	if event == "Stop" {
		return observeWorkActivity(store, sessionPath, sessionID, nil, true, nil)
	}
	return observeWorkActivity(store, sessionPath, sessionID, nil, event != "PostToolUse", func(context string) error {
		if event == "UserPromptSubmit" {
			context = strings.TrimSpace(activityInstructions + "\n\n" + context)
		}
		return deliver(context)
	})
}

func activityHook() error {
	payload, err := readHookPayload()
	if err != nil {
		fmt.Fprintf(os.Stderr, "myriad: work activity input unavailable: %v\n", err)
		return nil
	}
	// Activity failures are advisory and must never block the upstream hook.
	if err := writeActivityHookContext(os.Stdout, payload, ""); err != nil {
		fmt.Fprintf(os.Stderr, "myriad: work activity delivery unavailable: %v\n", err)
	}
	return nil
}

func writeActivityHookContext(output io.Writer, payload Record, prefix string) error {
	event := stringValue(payload, "hook_event_name")
	attempted := false
	err := activityHookContext(payload, func(context string) error {
		attempted = true
		return writeHookContext(output, event, strings.TrimSpace(prefix+"\n\n"+context))
	})
	if !attempted {
		if err != nil {
			fmt.Fprintf(os.Stderr, "myriad: work activity unavailable: %v\n", err)
		}
		// First-prompt provisioning context is still needed if activity fails.
		return writeHookContext(output, event, prefix)
	}
	return err
}
