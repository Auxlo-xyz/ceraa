package telegram

import (
	"strings"
	"testing"
	"time"
)

func TestRunCeilingDefaultsToGenerous(t *testing.T) {
	t.Setenv("CERAA_RUN_TIMEOUT", "")
	if got := runCeiling(); got != defaultRunCeiling {
		t.Errorf("default ceiling = %v, want %v", got, defaultRunCeiling)
	}
	// The old value killed long-horizon work, so the default must exceed it.
	if runCeiling() <= 10*time.Minute {
		t.Errorf("ceiling %v is still within the old 10 minute cap", runCeiling())
	}
}

func TestRunCeilingHonoursOverride(t *testing.T) {
	t.Setenv("CERAA_RUN_TIMEOUT", "90m")
	if got := runCeiling(); got != 90*time.Minute {
		t.Errorf("ceiling = %v, want 90m", got)
	}
}

// A typo must not leave a run unbounded, so a malformed or non-positive value
// falls back rather than disabling the guard.
func TestRunCeilingRejectsBadOverride(t *testing.T) {
	for _, bad := range []string{"soon", "0", "-5m", "10"} {
		t.Setenv("CERAA_RUN_TIMEOUT", bad)
		if got := runCeiling(); got != defaultRunCeiling {
			t.Errorf("CERAA_RUN_TIMEOUT=%q gave ceiling %v, want the default", bad, got)
		}
	}
}

func TestStopActiveRunCancelsAndReportsSession(t *testing.T) {
	var cancelled bool
	run := &activeRun{
		cancel:    func() { cancelled = true },
		stop:      func() { cancelled = true },
		sessionID: "sess-7",
	}
	activeRuns.Store(int64(42), run)
	t.Cleanup(func() { activeRuns.Delete(int64(42)) })

	stopped, sessionID := stopActiveRun(42)
	if !stopped {
		t.Fatal("stopActiveRun found no run")
	}
	if !cancelled {
		t.Error("the run was not cancelled")
	}
	if sessionID != "sess-7" {
		t.Errorf("session = %q, want sess-7", sessionID)
	}
}

func TestStopActiveRunWithNoRun(t *testing.T) {
	activeRuns.Delete(int64(99))
	if stopped, _ := stopActiveRun(99); stopped {
		t.Error("stopActiveRun claimed a run that does not exist")
	}
}

// A Stop button left over from a finished run must not cancel the next one.
func TestActiveRunMatchesGuardsStaleStopButton(t *testing.T) {
	activeRuns.Store(int64(43), &activeRun{
		cancel:    func() {},
		sessionID: "sess-new",
	})
	t.Cleanup(func() { activeRuns.Delete(int64(43)) })

	if activeRunMatches(43, "sess-old") {
		t.Error("a stale button matched the current run")
	}
	if !activeRunMatches(43, "sess-new") {
		t.Error("the current run did not match its own session")
	}
}

// The stop path must go through the renderer's stop, because that is what sets
// the stopped flag the run reports on.
func TestStopUsesRendererStopNotRawCancel(t *testing.T) {
	var stoppedFlag, cancelled bool
	run := &activeRun{
		cancel:    func() { cancelled = true },
		stop:      func() { stoppedFlag = true },
		sessionID: "s",
	}
	activeRuns.Store(int64(44), run)
	t.Cleanup(func() { activeRuns.Delete(int64(44)) })

	stopActiveRun(44)
	if !stoppedFlag {
		t.Error("renderer.stop was not called, so the run would report as a failure not a stop")
	}
	if cancelled {
		t.Error("raw cancel was called directly; stop() owns cancellation")
	}
}

func TestRunStopCallbackPrefixIsRecognised(t *testing.T) {
	// The callback data carries a session suffix, so the handler matches a prefix.
	data := "run:stop:tg-123"
	if !strings.HasPrefix(data, "run:stop") {
		t.Error("callback data does not match the handled prefix")
	}
	requested := strings.TrimPrefix(strings.TrimPrefix(data, "run:stop"), ":")
	if requested != "tg-123" {
		t.Errorf("session parsed as %q, want tg-123", requested)
	}
}
