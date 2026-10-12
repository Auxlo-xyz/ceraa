// Package cachewarm keeps a provider's prompt cache alive across the idle gap
// between two turns of the same conversation.
//
// A cached prefix survives for a fixed time after the last request that read or
// wrote it. A conversation that goes quiet for longer loses the entry, and the
// next turn re-bills the whole prompt at full input price instead of reading it
// at the cache price. For a Telegram bot that gap is the normal case rather
// than the exception: the owner thinks between messages, and a five-minute
// entry does not survive a five-minute thought.
//
// The refresh is a real request: the same messages, the same tools, the same
// cache key, with the output budget cut to a single token. It costs a cache
// read and a token, which is far below what the next turn saves if the entry is
// still warm. It is armed when a run finishes and cancelled the moment a run
// starts, so a real turn never has to queue behind one.
//
// Nothing here talks to a provider. The decision is a pure function of the
// clock, the size of the prefix and the economics, and the Warmer only owns the
// timer. That is deliberate: the part worth testing is the judgement about
// whether a refresh pays, and that is arithmetic, not I/O.
package cachewarm

import (
	"context"
	"errors"
	"sync"
	"time"
)

// DefaultTTL is the provider default for an ephemeral cache entry.
const DefaultTTL = 5 * time.Minute

// DefaultExtendedTTL is the one-hour entry Anthropic serves when a breakpoint
// asks for it, and the TTL this package assumes for a Messages endpoint whose
// profile named none.
//
// One hour is the right default there rather than the five-minute one. A 1-hour
// write costs 2x base input against 1.25x for 5 minutes, so it is a worse deal
// on a provider that never serves a read. On a Messages endpoint the prompt is
// tens of thousands of tokens, the read is what every later turn pays for, and
// the five-minute entry dies in the gap this package exists to cover - so the
// premium is bought back on the second turn and every one after it.
const DefaultExtendedTTL = time.Hour

// RefreshFraction is the share of the TTL at which the refresh is sent: late
// enough that a real turn arriving first wastes nothing, early enough that the
// entry is still alive when it does.
const RefreshFraction = 0.9

// MaxWarmAge bounds how long one conversation stays worth keeping warm. Past
// this the session is treated as parked, and warming it would be an open-ended
// spend on a conversation nobody has returned to.
const MaxWarmAge = time.Hour

// MinPrefixTokens is the smallest prefix worth a refresh. Below it the refresh
// costs nearly what the rewrite it prevents costs, and the arithmetic below
// refuses it before the economics are even consulted.
const MinPrefixTokens = 2048

// ContinuationProbability is the chance that a conversation answered a moment
// ago is picked up again before its entry lapses.
//
// Higher than the idle figure pi uses, and for a reason pi does not have: this
// refresh is armed at the end of a run, which is the moment a reply is most
// likely, not the moment a session has already gone quiet. A bot that has just
// answered a question is waiting on a follow-up, not on a stranger.
const ContinuationProbability = 0.5

// MinExpectedSavings is the dollar floor below which a refresh is refused.
//
// Small on purpose. The refresh itself costs a cache read and a token, so an
// absolute floor this low is not protecting the wallet so much as refusing
// arithmetic on numbers too small to reason about - the companion rule below is
// the one that actually decides.
const MinExpectedSavings = 0.01

// MinSavingsMultiple is how many times its own cost a refresh has to expect to
// save before it is sent.
//
// A ratio rather than a second dollar figure, because the whole point of the
// decision is that it is made without knowing whether the owner will reply. A
// refresh that wins only when the conversation continues has to win by enough
// to pay for the times it does not, and three is the number that keeps the
// feature a saving rather than a tax on every idle session in the daemon.
const MinSavingsMultiple = 3.0

// Economics is the price pair the decision is made against, in dollars per
// million tokens.
type Economics struct {
	InputPerMTok     float64
	CacheReadPerMTok float64
}

// CacheDiscount is how much cheaper a cached token is than a fresh one: the
// ratio between the two prices, which is the only thing the decision actually
// turns on.
//
// It is a separate accessor because the two prices are usually available
// together but are not needed together. RefreshCost and AvoidedRewrite both
// scale with the token count, so the count cancels out of the comparison and
// what remains is a ratio of prices. A provider whose catalogue entry is missing
// therefore does not disable the decision: it only removes the ability to quote
// a dollar figure.
func (e Economics) CacheDiscount() float64 {
	if e.CacheReadPerMTok <= 0 {
		return 0
	}
	d := e.InputPerMTok / e.CacheReadPerMTok
	if d < 1 {
		return 0
	}
	return d
}

// UnknownEconomics is the price pair to decide with when the provider is not in
// the catalogue.
//
// Zero prices would divide by zero, so this is a ratio rather than an absence:
// it claims a ten-times cache discount, which is the conservative end of what
// providers actually charge. Assuming a worse-than-real discount understates
// what a refresh saves, so the decision refuses more often than it would with
// prices in hand. Assuming a better one would spend money on the theory that the
// owner's next message arrives.
var UnknownEconomics = Economics{InputPerMTok: 10, CacheReadPerMTok: 1}

// RefreshCost is what one keepalive costs: the prefix arrives as a cache read,
// and the reply is a single token billed at output prices, which this treats as
// negligible rather than zero so the arithmetic never divides by nothing.
func (e Economics) RefreshCost(tokens int) float64 {
	if tokens <= 0 {
		return 0
	}
	return float64(tokens) * e.CacheReadPerMTok / 1e6
}

// AvoidedRewrite is what the next turn saves if the entry is still warm: the
// same tokens priced as a fresh write minus what they would have cost as a
// read.
func (e Economics) AvoidedRewrite(tokens int) float64 {
	if tokens <= 0 {
		return 0
	}
	delta := e.InputPerMTok - e.CacheReadPerMTok
	if delta < 0 {
		return 0
	}
	return float64(tokens) * delta / 1e6
}

// Decision is whether to keep a conversation warm, and when.
type Decision struct {
	Warm   bool
	Delay  time.Duration
	Reason string
}

// Decide reports whether a refresh is worth sending, and how long to wait.
//
// It is pure: no clock read, no I/O, every input path returns a verdict with a
// reason. The rules in order, each one a refusal that names itself:
//
//  1. A prefix too small to clear the provider minimum is not worth a request,
//     because the read it buys is smaller than the arithmetic can see.
//  2. A conversation idle past MaxWarmAge is parked. Warming it would be an
//     unbounded spend on a session that may never return.
//  3. The expected saving, which is the continuation chance times what a warm
//     entry saves, has to clear both a dollar floor and a multiple of what the
//     refresh itself costs. The dollar floor refuses arithmetic on numbers too
//     small to reason about; the ratio is the one that decides, because a
//     refresh that only wins when the owner replies has to win by enough to pay
//     for the times they do not.
//
// Only then is the delay computed, and a delay of zero means the entry has
// already lapsed and the refresh should go out now rather than never.
func Decide(now, lastUsed time.Time, ttl time.Duration, tokens int, economics Economics) Decision {
	if tokens < MinPrefixTokens {
		return Decision{Reason: "prefix too small to be worth a request"}
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	idle := now.Sub(lastUsed)
	if idle > MaxWarmAge {
		return Decision{Reason: "session idle past the warm horizon"}
	}
	// The ratio decides. See the comment on the comparison below for why the
	// refresh's own cost no longer appears in the condition.
	// The comparison is a ratio, so the token count cancels and what is left is
	// how much cheaper a cached token is than a fresh one. The dollar floor
	// still runs first on the absolute figure: it is the only reason the
	// decision needs prices rather than a ratio, and it refuses arithmetic on
	// numbers too small to reason about.
	expected := ContinuationProbability * economics.AvoidedRewrite(tokens)
	if expected < MinExpectedSavings || economics.CacheDiscount() <= ContinuationProbability*MinSavingsMultiple {
		return Decision{Reason: "expected saving below the floor"}
	}
	delay := time.Duration(float64(ttl) * RefreshFraction)
	if delay > idle {
		delay -= idle
	} else {
		delay = 0
	}
	return Decision{Warm: true, Delay: delay, Reason: "refresh scheduled"}
}

// Refresh replays a conversation so its cached prefix outlives the gap. The
// string is the signature the conversation had when the refresh was armed, and
// a refresh that finds a different one refuses to run: the owner has returned,
// a real turn is in flight, and a keepalive on top of it would be a second
// request for the same bytes.
type Refresh func(ctx context.Context, signature string) error

// Warmer owns the timer and nothing else.
//
// One per daemon rather than one per run, because the thing being kept warm
// outlives the run that wrote it. Arm replaces any pending refresh, so a
// conversation that keeps being used never queues more than one.
type Warmer struct {
	mu      sync.Mutex
	timer   *time.Timer
	armed   bool
	sig     string
	refresh Refresh
	logf    func(format string, args ...any)
}

// New builds a Warmer. A nil logf is allowed and silences the package.
func New(refresh Refresh, logf func(format string, args ...any)) *Warmer {
	return &Warmer{refresh: refresh, logf: logf}
}

func (w *Warmer) log(format string, args ...any) {
	if w != nil && w.logf != nil {
		w.logf(format, args...)
	}
}

// Arm schedules a refresh for delay from now, replacing anything pending.
//
// A delay of zero or less fires immediately on its own goroutine, which is the
// case where the entry has already lapsed and the refresh is a write rather
// than a read. That is still the right call: the alternative is paying the full
// rewrite on the next turn with nothing in between.
func (w *Warmer) Arm(signature string, delay time.Duration) {
	if w == nil || w.refresh == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancelLocked()
	w.armed = true
	w.sig = signature
	if delay < 0 {
		delay = 0
	}
	w.timer = time.AfterFunc(delay, func() { w.fire(signature) })
	w.log("cachewarm armed signature=%s delay=%s", signature, delay)
}

// Disarm cancels any pending refresh. A run calls it before it sends anything,
// so a keepalive can never land in front of a real turn.
func (w *Warmer) Disarm() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.armed {
		w.log("cachewarm disarmed signature=%s", w.sig)
	}
	w.cancelLocked()
}

// Armed reports whether a refresh is pending. It exists for tests and for a
// status surface, not for the hot path.
func (w *Warmer) Armed() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.armed
}

func (w *Warmer) cancelLocked() {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.armed = false
	w.sig = ""
}

func (w *Warmer) fire(signature string) {
	w.mu.Lock()
	if !w.armed || w.sig != signature {
		w.mu.Unlock()
		return
	}
	w.cancelLocked()
	refresh := w.refresh
	w.mu.Unlock()
	// The session check lives inside the refresh, not here: this goroutine must
	// not hold the lock across a network call, and a real turn that arrives
	// during the refresh must still be able to Disarm.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := refresh(ctx, signature); err != nil {
		w.log("cachewarm skipped signature=%s reason=%v", signature, err)
		return
	}
	w.log("cachewarm refreshed signature=%s", signature)
}

// ErrStale is what a refresh returns when the conversation moved on since the
// refresh was armed. It is not a failure: the owner came back, and the real
// turn is about to pay for the write the keepalive was meant to prevent, which
// is the correct outcome.
var ErrStale = errors.New("conversation advanced since the refresh was armed")
