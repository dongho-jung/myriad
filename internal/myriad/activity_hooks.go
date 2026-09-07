package myriad

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// A session plugin adds hooks without interpreting or rewriting caller settings.
func claudeActivityCommand(command []string, plugin string) []string {
	executable := commandExecutableIndex(command, "claude")
	if executable < 0 {
		return command
	}
	result := append([]string{}, command[:executable+1]...)
	result = append(result, "--plugin-dir", plugin)
	return append(result, command[executable+1:]...)
}

func materializeClaudeActivityPlugin(store *Store) (string, error) {
	launcher, err := materializeHookRuntime(store)
	if err != nil {
		return "", err
	}
	root := filepath.Dir(launcher)
	hooks := Record{}
	for _, event := range []string{"UserPromptSubmit", "PostToolUse", "Stop"} {
		hooks[event] = []any{Record{"hooks": []any{Record{
			"type": "command", "command": hookCommand(internalActivityHook, launcher), "timeout": 10,
		}}}}
	}
	files := map[string]Record{
		".claude-plugin/plugin.json": {"name": "myriad-work-activity", "description": "Work activity for this managed Myriad session"},
		"hooks/hooks.json":           {"hooks": hooks},
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
			return "", err
		}
		payload, err := json.Marshal(content)
		if err != nil {
			return "", err
		}
		if err := materializeHookFile(path, payload, 0o600); err != nil {
			return "", err
		}
	}
	return root, nil
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
