package coordinator

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"ceraa/harness/llm"
)

// stallLimit is the longest a provider may stay silent without producing
// anything the reader can use before the request is abandoned.
//
// The transport already fails a read that receives no bytes for the provider's
// idle window, but that watchdog counts bytes, and a byte is not an answer. A
// gateway that keeps the connection alive with SSE comments, role-only chunks
// or repeated usage reports resets it forever: the run waits with nothing to
// show, and on Telegram that is indistinguishable from the bot having died. The
// window is deliberately longer than the slowest legitimate think seen in
// production (a HIGH-reasoning Gemini model that took over two minutes to its
// first token) and shorter than a tool's own ceiling, so it only fires on a
// request that is genuinely not answering.
var stallLimit = 6 * time.Minute

// stallWatch cancels a streaming request that stops producing output.
//
// Only meaningful deltas count. An empty delta, a keepalive comment or a chunk
// carrying nothing but a role is traffic, not progress, so it neither extends
// the window nor is treated as the answer arriving.
type stallWatch struct {
	limit time.Duration
	kick  chan struct{}
	done  chan struct{}
	stop  sync.Once
	fired atomic.Bool
	// abandon cancels the request context. Held rather than captured by the
	// goroutine so Start stays usable from the call site that owns the stream.
	abandon context.CancelFunc
}

func newStallWatch(limit time.Duration) *stallWatch {
	return &stallWatch{
		limit: limit,
		kick:  make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
}

// Start arms the window and begins watching. A limit of zero disables the
// watchdog, which is what a caller that has its own deadline wants.
func (w *stallWatch) Start(cancel context.CancelFunc) {
	w.abandon = cancel
	if w.limit <= 0 {
		return
	}
	go w.watch()
}

func (w *stallWatch) watch() {
	timer := time.NewTimer(w.limit)
	defer timer.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-w.kick:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(w.limit)
		case <-timer.C:
			w.fired.Store(true)
			w.abandon()
			return
		}
	}
}

// Kick records that the request produced something usable. It never blocks: a
// burst of deltas needs one reset, not one per delta.
func (w *stallWatch) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Stop releases the goroutine. Safe to call more than once and before Start.
func (w *stallWatch) Stop() {
	w.stop.Do(func() { close(w.done) })
}

// Err is the failure to report, or nil when the request did not stall.
func (w *stallWatch) Err() error {
	if !w.fired.Load() {
		return nil
	}
	return fmt.Errorf("provider stalled: no output for %s; the connection stayed open but the answer never arrived. Send the message again to retry", w.limit)
}

// countsAsProgress reports whether a delta is output the run can use.
//
// Content, reasoning and tool calls each move the answer forward. Usage is
// metadata about a request that may have produced nothing, so a provider
// repeating it forever is still silent as far as the reader is concerned.
func countsAsProgress(delta llm.ProviderResponse) bool {
	return delta.Content != "" ||
		delta.Reasoning != "" ||
		len(delta.ToolCalls) > 0 ||
		len(delta.ReasoningDetails) > 0
}
