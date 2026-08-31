package myriad

import (
	"reflect"
	"testing"
)

func TestCheckTimeoutRejectsInvalidDurations(t *testing.T) {
	for _, value := range []string{"NaN", "+Inf", "1e300", "1e-300", "0", "-1"} {
		if _, err := parseLaunchOptions([]string{"--check-timeout", value}); err == nil {
			t.Errorf("launch accepted check timeout %q", value)
		}
		if _, err := parseResumeOptions([]string{"--check-timeout", value}); err == nil {
			t.Errorf("resume accepted check timeout %q", value)
		}
	}
}

func TestCheckTimeoutAcceptsFractionalSeconds(t *testing.T) {
	options, err := parseLaunchOptions([]string{"--check-timeout", "0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if options.CheckTimeout != 0.1 {
		t.Fatalf("check timeout = %g, want 0.1", options.CheckTimeout)
	}
}

func TestDefaultChatResumePreservesCodexDirectoryScope(t *testing.T) {
	base := []string{"codex", "resume", "--dangerously-bypass-approvals-and-sandbox", "-c", `tui.resume_cwd="current"`}
	tests := []struct {
		name                  string
		sessionID             string
		last                  bool
		all                   bool
		includeNonInteractive bool
		want                  []string
	}{
		{name: "picker", want: base},
		{name: "last", last: true, want: appendCopy(base, "--last")},
		{name: "all", all: true, want: appendCopy(base, "--all")},
		{name: "all last", last: true, all: true, want: appendCopy(base, "--all", "--last")},
		{name: "session", sessionID: "session-id", want: appendCopy(base, "session-id")},
		{name: "non-interactive", includeNonInteractive: true, want: appendCopy(base, "--include-non-interactive")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := defaultChatResumeCommand(test.sessionID, test.last, test.all, test.includeNonInteractive)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("resume command = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestDefaultChatResumeRejectsConflictingSelectors(t *testing.T) {
	for _, options := range []struct {
		last bool
		all  bool
	}{{last: true}, {all: true}, {last: true, all: true}} {
		if _, err := defaultChatResumeCommand("session-id", options.last, options.all, false); err == nil {
			t.Fatalf("session id with last=%t all=%t was accepted", options.last, options.all)
		}
	}
}

func TestPassthroughChatResumeDoesNotBroadenScope(t *testing.T) {
	base := []string{"codex", "resume", "--dangerously-bypass-approvals-and-sandbox", "-c", `tui.resume_cwd="current"`}
	if got := passthroughChatResumeCommand(nil); !reflect.DeepEqual(got, base) {
		t.Fatalf("resume command = %#v, want %#v", got, base)
	}
	want := appendCopy(base, "--all")
	if got := passthroughChatResumeCommand([]string{"--all"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit all command = %#v, want %#v", got, want)
	}
}

func appendCopy(values []string, additions ...string) []string {
	result := append([]string{}, values...)
	return append(result, additions...)
}
