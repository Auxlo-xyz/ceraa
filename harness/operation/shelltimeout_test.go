package operation

import (
	"testing"
	"time"
)

func TestClampShellTimeout(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		want    time.Duration
	}{
		{60, 60 * time.Second},
		{900, 900 * time.Second},
		{3600, 3600 * time.Second},
		{3601, maxShellTimeout},
		{86400, maxShellTimeout},
	} {
		if got := clampShellTimeout(tc.seconds); got != tc.want {
			t.Errorf("clampShellTimeout(%d) = %v, want %v", tc.seconds, got, tc.want)
		}
	}
}

// The old default was 60s, shorter than a cold build on this host, so ordinary
// work was killed mid-command.
func TestDefaultShellTimeoutExceedsTheOldCap(t *testing.T) {
	if defaultShellTimeout <= 60*time.Second {
		t.Errorf("default shell timeout %v is still the old 60s cap", defaultShellTimeout)
	}
	if defaultShellTimeout <= 10*time.Minute {
		t.Errorf("default shell timeout %v leaves no room for a test suite", defaultShellTimeout)
	}
}

// SpecTimeout is the single source of truth for how long an operation may run:
// the executor uses it and so does the caller that waits on the batch. These
// cases pin each branch so a new operation type cannot quietly fall back to a
// shorter wait than the work it dispatched.
func TestSpecTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
		want time.Duration
	}{
		{"shell without an argument uses the shell default", Spec{Type: "shell"}, defaultShellTimeout},
		{"shell honors the argument", Spec{Type: "shell", Args: map[string]any{"timeout": 600}}, 600 * time.Second},
		{"shell clamps the argument", Spec{Type: "shell", Args: map[string]any{"timeout": 86400}}, maxShellTimeout},
		{"secret shell without an argument uses the shell default", Spec{Type: "run_command_with_secret"}, defaultShellTimeout},
		{"secret shell honors the argument", Spec{Type: "run_command_with_secret", Args: map[string]any{"timeout": 5}}, 5 * time.Second},
		{"secret shell clamps the argument", Spec{Type: "run_command_with_secret", Args: map[string]any{"timeout": 86400}}, maxShellTimeout},
		{"task uses its child wall clock", Spec{Type: "task"}, TaskTimeout},
		{"question waits for a human", Spec{Type: "question"}, questionTimeout},

		{"wire tool honors the argument", Spec{Type: "webfetch", Args: map[string]any{"timeout": 90}}, 90 * time.Second},
		{"in-process tool without an argument uses the runner default", Spec{Type: "browser_open"}, defaultRunnerTimeout},
		{"in-process tool honors the argument", Spec{Type: "browser_open", Args: map[string]any{"timeout": 240}}, 240 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SpecTimeout(tc.spec); got != tc.want {
				t.Errorf("SpecTimeout(%+v) = %v, want %v", tc.spec, got, tc.want)
			}
		})
	}
}

// A spec with no arguments map at all is the common defensive case, and must not
// panic on the nil lookup.
func TestSpecTimeoutToleratesNilArgs(t *testing.T) {
	if got := SpecTimeout(Spec{Type: "shell"}); got != defaultShellTimeout {
		t.Errorf("SpecTimeout with nil Args = %v, want %v", got, defaultShellTimeout)
	}
}
