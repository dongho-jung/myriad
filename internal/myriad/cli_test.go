package myriad

import "testing"

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
