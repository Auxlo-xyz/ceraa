package compaction

import (
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

func compactorFor(window, maxOutput int) *Compactor {
	return &Compactor{
		Settings:        DefaultSettings(),
		ContextLimit:    window,
		MaxOutputTokens: maxOutput,
	}
}

// Compaction used to subtract a flat buffer from a limit that the resolver had
// already reduced, so the same reservation was counted twice. The result was not
// a percentage at all: a 200k model compacted at 80% of its window and a 32k one
// at 12%, neither of which was ever chosen.
func TestCompactAtIsNinetyPercentOfAnyGenerousWindow(t *testing.T) {
	for _, window := range []int{200000, 512000, 1000000, 2000000} {
		c := compactorFor(window, 8192)
		want := window * 90 / 100
		if got := c.compactAt(); got != want {
			t.Errorf("window %d: compactAt = %d, want %d (90%%)", window, got, want)
		}
	}
}

// A window too small to hold the absolute reserve must still trigger at a sane
// point. The raw difference is negative on a small window, and a negative
// threshold is exceeded by every turn, so compaction would run forever and never
// reclaim anything.
// The reported bug, end to end. A split-reporting endpoint reports the cache read
// beside the fresh input, so the turn's TotalTokens carries the fresh tokens only.
// This session's last turn re-read a 185k prefix and reported 2,700 tokens; the
// trigger compared that against 90% of the window and stayed silent while the real
// prompt was over 187k.
func TestNeedsCompactionCountsTheCachedPrefix(t *testing.T) {
	usage := llm.UsageInfo{InputTokens: 1200, CachedInputTokens: 185000, OutputTokens: 1500, TotalTokens: 2700}
	sess := &sessionstore.Session{Messages: []sessionstore.Message{{Role: llm.RoleAssistant, Usage: usage}}}
	c := compactorFor(200000, 8192)
	if usage.TotalTokens >= c.compactAt() {
		t.Fatalf("this fixture no longer separates TotalTokens (%d) from ContextTokens; pick another", usage.TotalTokens)
	}
	want := 1200 + 185000 + 1500
	if got := EstimatedContext(sess); got != want {
		t.Fatalf("estimatedContext = %d, want %d; the cached prefix is part of the prompt", got, want)
	}
	if !c.NeedsCompaction(sess) {
		t.Fatal("a session whose prompt is 187.7k of a 200k window did not compact")
	}
}

func TestCompactAtStaysPositiveOnASmallWindow(t *testing.T) {
	for _, window := range []int{4000, 8000, 16000, 32000} {
		c := compactorFor(window, 8192)
		got := c.compactAt()
		if got <= 0 {
			t.Errorf("window %d: compactAt = %d, must be positive", window, got)
		}
		if got > window {
			t.Errorf("window %d: compactAt = %d exceeds the window", window, got)
		}
	}
}

// The trigger must leave room for the reply. A model that compacts at 95% of a
// 32k window has 1.6k tokens left to answer in, which is less than it was asked
// to produce.
func TestCompactAtLeavesRoomForTheReply(t *testing.T) {
	const maxOutput = 8192
	for _, window := range []int{32000, 64000, 200000} {
		c := compactorFor(window, maxOutput)
		if room := window - c.compactAt(); room < maxOutput {
			t.Errorf("window %d: only %d tokens left after the trigger, want at least %d",
				window, room, maxOutput)
		}
	}
}

// The end-to-end property: a session is compacted once the trigger is reached and
// not before.
func TestNeedsCompactionRespectsTheTrigger(t *testing.T) {
	window := 200000
	c := compactorFor(window, 8192)
	at := c.compactAt()
	if at != 180000 {
		t.Fatalf("compactAt = %d, want 180000", at)
	}
	// Synthesise usage just under and just over the trigger.
	under := sessionWithUsage(t, at-1000)
	if c.NeedsCompaction(under) {
		t.Errorf("compacted at %d tokens with a trigger of %d", at-1000, at)
	}
	over := sessionWithUsage(t, at+1000)
	if !c.NeedsCompaction(over) {
		t.Errorf("did not compact at %d tokens with a trigger of %d", at+1000, at)
	}
}

// A 200k window cannot tell the two trigger formulas apart: 90% of 200k and
// 200k minus the old flat buffer are both 180000, so a test there passes against
// either implementation. A 512k window separates them by 31k, and this asserts
// through NeedsCompaction rather than compactAt so that unwiring the helper is
// caught too.
func TestNeedsCompactionFiresAtNinetyPercentOnALargeWindow(t *testing.T) {
	const window = 512000
	c := compactorFor(window, 8192)
	at := window * 90 / 100
	if at == window-DefaultBuffer {
		t.Fatal("this window no longer distinguishes the trigger formulas; pick another")
	}
	// Just past 90%: the old formula would still say there is room, because
	// 200k of headroom on a 512k window is a third of the model.
	if !c.NeedsCompaction(sessionWithUsage(t, at+1)) {
		t.Errorf("did not compact just past 90%% of a %d window; the trigger is not the 90%% one", window)
	}
	// Just under: still room.
	if c.NeedsCompaction(sessionWithUsage(t, at-1)) {
		t.Errorf("compacted just under 90%% of a %d window", window)
	}
}

// A larger configured buffer is a deliberate choice and must still be honoured,
// even though it is above the built-in reserve.
func TestConfiguredBufferRaisesTheTrigger(t *testing.T) {
	c := compactorFor(200000, 8192)
	c.Settings.Buffer = 40000
	if got, want := c.compactAt(), 200000-40000; got != want {
		t.Errorf("compactAt = %d, want %d to honour the configured buffer", got, want)
	}
}

// The resolver must not shave the window any more. A caller that needs headroom
// subtracts it for itself, and doing it here as well was the double count.
func TestResolvedWindowIsTheWindowNotLess(t *testing.T) {
	for _, window := range []int{8000, 32000, 200000, 1000000} {
		// Configured equal to the advertised window: the limit governs and is
		// returned whole rather than shaved for headroom.
		got, source := llm.ResolveContextWindow(window, llm.ModelMetadata{ContextWindow: window})
		if got != window {
			t.Errorf("window %d resolved to %d, want the window unchanged", window, got)
		}
		if source != llm.ContextFromProfile {
			t.Errorf("window %d: source = %s", window, source)
		}
	}
}

func sessionWithUsage(t *testing.T, tokens int) *sessionstore.Session {
	t.Helper()
	return &sessionstore.Session{Messages: []sessionstore.Message{{
		Role:  llm.RoleAssistant,
		Usage: llm.UsageInfo{TotalTokens: tokens},
	}}}
}
