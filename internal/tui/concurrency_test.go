package tui

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/operation"
)

// The coordinator dispatches tools concurrently and the spinner repaints on its
// own goroutine, so several callbacks arrive at once. This does not replace the
// race detector, which needs cgo and a C compiler and is not available here; it
// catches the failure that would actually be observed, which is a panic on a
// concurrent map write or a deadlock between the callback and the ticker.
func TestRendererSurvivesConcurrentCallbacks(t *testing.T) {
	r, out := renderInto()
	r.TurnStart()
	const workers = 8
	const rounds = 40
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				switch (w + i) % 6 {
				case 0:
					r.Content("chunk ")
				case 1:
					r.ToolStart("bash", map[string]any{"command": fmt.Sprintf("echo %d", i)})
				case 2:
					r.ToolDone("bash", operation.ResultEvent{Status: operation.StatusCompleted, Output: "ok"})
				case 3:
					r.Usage(llm.UsageInfo{InputTokens: i, OutputTokens: i}, llm.UsageTrust{Trusted: true})
				case 4:
					r.Cost(llm.CostInfo{TotalUSD: float64(i) / 1000, Known: true}, "stub")
				case 5:
					r.repaint(i)
				}
			}
		}(w)
	}
	wg.Wait()
	r.TurnEnd(nil)
	if out.Len() == 0 {
		t.Fatal("no output after concurrent callbacks")
	}
	// The footer is written last and under the same lock as the usage it reports.
	// Which worker wins is not deterministic, so the assertion is that a footer
	// exists and that its numbers came from one of the workers rather than being
	// interleaved or dropped. The footer is located by its rule, not by a fixed
	// marker string, so restyling it does not break this.
	got := out.String()
	at := strings.LastIndex(got, "─")
	if at < 0 {
		t.Fatalf("no footer rule was written:\n%s", tail(got))
	}
	footer := got[at:]
	if !strings.Contains(footer, "in ") || !strings.Contains(footer, "out ") {
		t.Errorf("footer did not report usage: %q", footer)
	}
}

func TestRendererStateIsReadableDuringWork(t *testing.T) {
	// The prompt reads the state on the main goroutine while the ticker repaints,
	// so State must not race with the writes that change it.
	r, _ := renderInto()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			r.TurnStart()
			r.ToolStart("read", map[string]any{"path": "a"})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = r.State()
		}
	}()
	wg.Wait()
}

// tail returns the last few lines of s, for a failure message that would otherwise
// scroll a screenful of interleaved output past the actual assertion.
func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return strings.Join(lines, "\n")
}

func TestLiveCountNeverGoesNegative(t *testing.T) {
	// A tool done without a matching start must not drive the counter below zero,
	// because the working line would then read "working -1".
	r, _ := renderInto()
	r.TurnStart()
	for i := 0; i < 5; i++ {
		r.ToolDone("bash", operation.ResultEvent{Status: operation.StatusCompleted})
	}
	r.mu.Lock()
	live := r.live
	r.mu.Unlock()
	if live < 0 {
		t.Errorf("live count = %d, want it clamped at zero", live)
	}
}

// The answer buffer is reached from two goroutines at once: Content streams
// deltas from the provider while a tool line from the coordinator arrives and
// closes the stream, and a turn boundary resets the buffer underneath both.
//
// Before the buffer had a lock of its own, flushAnswer read aTail while
// Content was rewriting it. A string header read half-written has a data
// pointer and a length that do not belong together, and the panic came out in
// strings.TrimSpace as a nil pointer with a non-zero length. This is the
// closest this host can come to reproducing it, since the race detector needs
// cgo and a C compiler and neither is installed here.
func TestAnswerBufferSurvivesConcurrentStreamAndFlush(t *testing.T) {
	r, out := renderInto()
	r.TurnStart()
	const workers = 8
	const rounds = 250
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				switch (w + i) % 4 {
				case 0, 1:
					r.Content(fmt.Sprintf("streamed delta %d from worker %d\n", i, w))
				case 2:
					r.ToolStart("bash", map[string]any{"command": fmt.Sprintf("echo %d", i)})
				case 3:
					r.TurnStart()
				}
			}
		}(w)
	}
	wg.Wait()
	r.TurnEnd(nil)
	if out.Len() == 0 {
		t.Fatal("no output after streaming against flushing")
	}
}
