package myriad

import (
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

const maxFuzzArgumentBytes = 4096

func fuzzArgv(payload []byte) []string {
	if len(payload) == 0 {
		return nil
	}
	if len(payload) > maxFuzzArgumentBytes {
		payload = payload[:maxFuzzArgumentBytes]
	}
	arguments := strings.Split(string(payload), "\x00")
	if len(arguments) > 64 {
		arguments = arguments[:64]
	}
	return arguments
}

func FuzzCommandInterpretation(f *testing.F) {
	for _, seed := range [][]byte{
		{},
		[]byte("codex"),
		[]byte("codex\x00-c"),
		[]byte("codex\x00resume\x00--last"),
		[]byte("codex\x00--\x00--remote\x00unix:///tmp/server.sock"),
		[]byte("env\x00-uCODEX_TOKEN\x00codex\x00--remote\x00unix:///tmp/server.sock"),
		[]byte("env\x00-S\x00codex --remote unix:///tmp/server.sock"),
		[]byte("env\x00-\x00TOKEN=value\x00--\x00codex"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		command := fuzzArgv(payload)
		original := append([]string{}, command...)

		executable := commandExecutableIndex(command, "codex")
		if executable < -1 || executable >= len(command) {
			t.Fatalf("executable index %d is outside command of length %d", executable, len(command))
		}
		if executable >= 0 && filepath.Base(command[executable]) != "codex" {
			t.Fatalf("executable index %d points to %q", executable, command[executable])
		}

		subcommand := codexSubcommand(command)
		if subcommand != "" && !codexSubcommands[subcommand] {
			t.Fatalf("unknown subcommand %q", subcommand)
		}
		_ = freshInteractiveCodexCommand(command)
		_ = interactiveCodexCommand(command)
		_ = codexUsesRemoteAppServer(command)
		_ = claudeDirectInvocation(command)
		_ = validateForegroundAgentCommand("codex", command, true)
		_ = validateForegroundAgentCommand("claude", command, true)

		end, err := codexGlobalArgumentsEnd(command)
		if executable < 0 && err == nil {
			t.Fatal("global argument parser accepted a command without codex")
		}
		if err == nil && (end <= executable || end > len(command)) {
			t.Fatalf("global argument end %d is invalid for executable %d and length %d", end, executable, len(command))
		}
		if executable >= 0 {
			_ = stripManagedCodexTUIConfigs(command, executable)
		}
		_ = codexRemoteCommand(command, "/tmp/myriad-fuzz.sock", []string{"/tmp/myriad-fuzz"}, codexStatusLine, "/tmp/myriad-fuzz")

		if !slices.Equal(command, original) {
			t.Fatalf("command interpretation mutated its input: %#v became %#v", original, command)
		}
	})
}

func FuzzCLIOptionParsing(f *testing.F) {
	for _, seed := range [][]byte{
		{},
		[]byte("--agent\x00codex\x00description"),
		[]byte("--check-timeout\x000.1\x00--\x00codex\x00resume"),
		[]byte("--target"),
		[]byte("--last\x00--\x00--all"),
		[]byte("session-one\x00session-two"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		arguments := fuzzArgv(payload)
		_, command := splitCommand(arguments)

		launch, launchErr := parseLaunchOptions(arguments)
		if launchErr == nil {
			if launch.Agent != "codex" && launch.Agent != "claude" && launch.Agent != "custom" {
				t.Fatalf("launch parser returned unsupported agent %q", launch.Agent)
			}
			if _, ok := durationFromSeconds(launch.CheckTimeout); !ok {
				t.Fatalf("launch parser returned invalid timeout %v", launch.CheckTimeout)
			}
			if !reflect.DeepEqual(launch.Command, command) {
				t.Fatalf("launch command = %#v, want %#v", launch.Command, command)
			}
		}

		resume, resumeErr := parseResumeOptions(arguments)
		if resumeErr == nil {
			if _, ok := durationFromSeconds(resume.CheckTimeout); !ok {
				t.Fatalf("resume parser returned invalid timeout %v", resume.CheckTimeout)
			}
			if !reflect.DeepEqual(resume.Arguments, command) {
				t.Fatalf("resume arguments = %#v, want %#v", resume.Arguments, command)
			}
		}
	})
}

func FuzzStateDecoding(f *testing.F) {
	for _, seed := range []string{
		`{}`,
		`{"schema_version":1,"task_id":"task-one"}`,
		`{"schema_version":1,"settings":{"integration_target":"main"},"memories":{"a":{"summary":"value"}}}`,
		`{"schema_version":1,"session_id":"session-one","messages":[]}`,
		`{"jira_issues":["CAPE-1"],"pull_request_numbers":[1,2]}`,
		`{"schema_version":1} trailing`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > 64*1024 {
			payload = payload[:64*1024]
		}
		var value Record
		if err := decodeJSON(payload, &value); err != nil {
			return
		}
		_ = validateTaskRecord(value, stringValue(value, "task_id"))
		_ = validateMemory(value)
		_ = validateInbox(value, stringValue(value, "session_id"))
		_ = normalizeTaskContext(cloneRecord(value))
	})
}

func FuzzStatuslineRendering(f *testing.F) {
	f.Add("myriad", 24, uint64(0))
	f.Add("한글과 emoji 🚀", 12, math.Float64bits(math.NaN()))
	f.Add(strings.Repeat("x", 256), 1001, math.Float64bits(math.Inf(1)))
	f.Fuzz(func(t *testing.T, label string, width int, epochBits uint64) {
		if len(label) > maxFuzzArgumentBytes {
			label = label[:maxFuzzArgumentBytes]
		}
		tasks := []Record{
			{"task_id": "current", "agent": label, "repository": filepath.Join("/tmp", label), "created_at": "2"},
			{"task_id": "other", "agent": "claude", "repository": filepath.Join("/tmp", label+"-other"), "created_at": "1"},
		}
		effectiveWidth := width
		if effectiveWidth < 12 {
			effectiveWidth = 12
		}
		if effectiveWidth > 1000 {
			effectiveWidth = 1000
		}
		line := renderStatusline(tasks, "current", width, math.Float64frombits(epochBits), compactASCII(label, "extra", 100))
		if !utf8.ValidString(line) {
			t.Fatalf("statusline is not valid UTF-8: %q", line)
		}
		if got := len([]rune(line)); got > effectiveWidth {
			t.Fatalf("statusline width %d exceeds limit %d: %q", got, effectiveWidth, line)
		}
	})
}
