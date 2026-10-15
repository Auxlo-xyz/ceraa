package compaction

import (
	"testing"
	"time"
)

// A session idle a minute on an hour TTL must keep its entry: folding now
// would spend a summarisation request to replace a cache that has not lapsed.
func TestIdleCompactLeavesAFreshSessionAlone(t *testing.T) {
	now := time.Now()
	got := DecideIdleCompact(now, now.Add(-time.Minute), time.Hour, 60000, 60000, true)
	if got.Fold {
		t.Errorf("folded a session idle a minute: %s", got.Reason)
	}
}

// The property the feature exists for: idle past 90% of the TTL, large, with
// new content since any fold. The summariser's read is still a cache read, and
// the returning turn reprocesses the summary instead of the conversation.
func TestIdleCompactFoldsAtTheCacheSeam(t *testing.T) {
	now := time.Now()
	got := DecideIdleCompact(now, now.Add(-55*time.Minute), time.Hour, 60000, 60000, true)
	if !got.Fold {
		t.Errorf("did not fold a session idle 55m of a 1h TTL: %s", got.Reason)
	}
}

// Auto off means off everywhere. An operator who disabled compaction did not
// ask for a background sweeper to second-guess it.
func TestIdleCompactRespectsAutoOff(t *testing.T) {
	now := time.Now()
	got := DecideIdleCompact(now, now.Add(-2*time.Hour), time.Hour, 60000, 60000, false)
	if got.Fold {
		t.Errorf("folded with auto compaction off: %s", got.Reason)
	}
}

// Folding a small session spends a summarisation request to save almost
// nothing: the fold keeps eight thousand tokens plus a summary, so below the
// floor the before and after are nearly the same size.
func TestIdleCompactRefusesASmallSession(t *testing.T) {
	now := time.Now()
	got := DecideIdleCompact(now, now.Add(-2*time.Hour), time.Hour, IdleMinTokens-1, IdleMinTokens, true)
	if got.Fold {
		t.Errorf("folded a session below the floor: %s", got.Reason)
	}
}

// The idempotency rule: after a fold the post-checkpoint content is small, so
// the same idle episode never folds twice, across sweeps and across restarts,
// with no persisted cursor to keep.
func TestIdleCompactFoldsOneIdleEpisodeOnce(t *testing.T) {
	now := time.Now()
	lastActive := now.Add(-2 * time.Hour)
	if got := DecideIdleCompact(now, lastActive, time.Hour, 60000, MinFoldGrowthTokens, true); !got.Fold {
		t.Fatalf("did not fold %s", got.Reason)
	}
	if got := DecideIdleCompact(now, lastActive, time.Hour, 60000, MinFoldGrowthTokens-1, true); got.Fold {
		t.Errorf("folded twice with no new content: %s", got.Reason)
	}
}

// No known lifetime means no known seam. Folding anyway would spend on a guess
// about when the entry lapses.
func TestIdleCompactRefusesWithoutACacheLifetime(t *testing.T) {
	now := time.Now()
	got := DecideIdleCompact(now, now.Add(-2*time.Hour), 0, 60000, 60000, true)
	if got.Fold {
		t.Errorf("folded with no cache lifetime: %s", got.Reason)
	}
}

// A file mtime in the future is clock skew, not idleness. Clamp it to now
// rather than folding a session that just spoke.
func TestIdleCompactTreatsAFutureTimestampAsNow(t *testing.T) {
	now := time.Now()
	got := DecideIdleCompact(now, now.Add(time.Minute), time.Hour, 60000, 60000, true)
	if got.Fold {
		t.Errorf("folded a session active in the future: %s", got.Reason)
	}
}

// The seam is 90% of whatever TTL the provider serves, not a hardcoded hour: a
// five-minute entry lapses long before an hourly sweep would notice it.
func TestIdleCompactSeamFollowsTheProviderTTL(t *testing.T) {
	now := time.Now()
	if got := DecideIdleCompact(now, now.Add(-4*time.Minute), 5*time.Minute, 60000, 60000, true); got.Fold {
		t.Errorf("folded at 80%% of a 5m TTL: %s", got.Reason)
	}
	if got := DecideIdleCompact(now, now.Add(-5*time.Minute), 5*time.Minute, 60000, 60000, true); !got.Fold {
		t.Errorf("did not fold past 90%% of a 5m TTL: %s", got.Reason)
	}
}
