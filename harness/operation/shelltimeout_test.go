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
