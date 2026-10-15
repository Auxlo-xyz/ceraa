package compaction

import (
	"time"

	"ceraa/harness/sessionstore"
)

// IdleCompactFraction is the share of the prompt-cache TTL at which an idle
// session is folded.
//
// It is the same seam the keepalive refreshes at (cachewarm.RefreshFraction),
// and for the same reason from the other direction: the summariser's request
// replays the live conversation, so while the entry is still alive that whole
// read is a cache read rather than a full-price rewrite. Folding after the
// lapse still helps every later turn, but folding before it also makes the
// fold itself cheap.
const IdleCompactFraction = 0.9

// IdleMinTokens is the smallest estimated context worth an idle fold.
//
// A fold keeps Keep tokens plus a summary, so folding a small session spends a
// summarisation request to save almost nothing on the next turn. Thirty
// thousand is where the folded session (about twelve thousand) is less than
// half the unfolded one, so the saving survives the cost of the fold even when
// the owner returns only once.
const IdleMinTokens = 30000

// IdleDecision is whether an idle session should be folded now, and why not.
type IdleDecision struct {
	Fold   bool
	Reason string
}

// DecideIdleCompact reports whether a session last active at lastActive should
// be folded at now.
//
// It is pure: no clock read, no I/O, every input path returns a verdict with
// a reason. The rules in order, each one a refusal that names itself:
//
//  1. Auto compaction off means off everywhere, including here.
//  2. A session below the fold floor is not worth a summarisation request.
//  3. Content younger than the last fold below the growth gate means this idle
//     episode already folded: folding again would spend a request to fold the
//     same history, which is also what makes the sweeper idempotent across
//     restarts without any persisted cursor.
//  4. No known cache lifetime means no known seam, so no fold. Never spend on
//     a guess about when the entry lapses.
//  5. A session idle short of the seam keeps its cache entry and is left alone.
func DecideIdleCompact(now, lastActive time.Time, ttl time.Duration, tokens, growth int, auto bool) IdleDecision {
	if !auto {
		return IdleDecision{Reason: "auto compaction off"}
	}
	if tokens < IdleMinTokens {
		return IdleDecision{Reason: "session below the idle fold floor"}
	}
	if growth < MinFoldGrowthTokens {
		return IdleDecision{Reason: "no new content since the last fold"}
	}
	if ttl <= 0 {
		return IdleDecision{Reason: "no cache lifetime known"}
	}
	idle := now.Sub(lastActive)
	if idle < 0 {
		idle = 0
	}
	if idle < time.Duration(float64(ttl)*IdleCompactFraction) {
		return IdleDecision{Reason: "cache entry still fresh"}
	}
	return IdleDecision{Fold: true, Reason: "idle past the cache seam"}
}

// GrowthSinceCheckpoint estimates the tokens arrived after the latest
// checkpoint, in the same measure the fold itself uses.
//
// Exported for the idle sweeper, which lives outside this package: it is the
// idempotency half of the idle decision, refusing to fold one idle episode
// twice.
func (c *Compactor) GrowthSinceCheckpoint(sess *sessionstore.Session) int {
	return growthSinceCheckpoint(sess, c.replaysReasoning())
}
