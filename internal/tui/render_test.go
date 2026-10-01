package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/operation"
)

// renderInto builds a plain renderer over a buffer.
//
// A plain palette is used so the assertions are about content and layout rather
// than about escape sequences, and the colour behaviour has its own tests.
func renderInto() (*Renderer, *strings.Builder) {
	var b strings.Builder
	return NewRenderer(&b, Plain()), &b
}

func TestContentIsWrittenThroughUnbuffered(t *testing.T) {
	r, out := renderInto()
	r.Content("hello ")
	r.Content("world")
	got := out.String()
	// The answer is preceded by a blank line so it is separated from the prompt,
	// which is the only thing allowed before the text.
	if !strings.HasPrefix(got, "\n") {
		t.Fatalf("answer does not start on a fresh line: %q", got)
	}
	if !strings.HasSuffix(got, "hello world") {
		t.Fatalf("content = %q, want the deltas in order", got)
	}
}

func TestContentOpensExactlyOnce(t *testing.T) {
	r, out := renderInto()
	// With colour off the open is empty, so the newline that starts the answer is
	// the observable marker and it must appear once, not per delta.
	r.Content("a")
	r.Content("b")
	r.Content("c")
	if got := strings.Count(out.String(), "\n"); got != 1 {
		t.Fatalf("answer body produced %d newlines, want 1: %q", got, out.String())
	}
}

func TestToolStartDoesNotDumpWholeArguments(t *testing.T) {
	// The failure this guards is real: a content argument is an entire file, and
	// rendering it puts the file in the transcript.
	huge := strings.Repeat("A", 50000)
	r, out := renderInto()
	r.ToolStart("write", map[string]any{"path": "a.go", "content": huge})
	got := out.String()
	if strings.Contains(got, huge[:200]) {
		t.Fatal("tool start rendered the content argument")
	}
	if len(got) > 400 {
		t.Fatalf("tool start line is %d bytes, expected a bounded summary: %q", len(got), got)
	}
	if !strings.Contains(got, "a.go") {
		t.Errorf("tool start did not name the path: %q", got)
	}
}

func TestToolArgsSummaryPicksTheIdentifyingArgument(t *testing.T) {
	// Go map order is random, so an implementation that took "whichever key came
	// first" would pass intermittently and fail in production.
	for i := 0; i < 50; i++ {
		got := ToolArgsSummary("bash", map[string]any{
			"zzz": "ignored", "command": "go test ./...", "aaa": "ignored",
		})
		if got != "go test ./..." {
			t.Fatalf("bash summary = %q, want the command", got)
		}
	}
}

func TestToolArgsSummaryBoundsTheValue(t *testing.T) {
	got := ToolArgsSummary("bash", map[string]any{"command": strings.Repeat("x", 500)})
	if len(got) > 100 {
		t.Fatalf("summary is %d chars, expected it bounded", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("a truncated summary should say so, got %q", got)
	}
}

func TestToolArgsSummaryCollapsesNewlines(t *testing.T) {
	got := ToolArgsSummary("bash", map[string]any{"command": "line one\nline two"})
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("summary kept a newline and would break the line: %q", got)
	}
}

func TestToolDoneMarksFailureDistinctly(t *testing.T) {
	r, out := renderInto()
	r.ToolDone("bash", operation.ResultEvent{Status: operation.StatusCompleted, Output: "done"})
	if !strings.Contains(out.String(), glyph("ok")) {
		t.Errorf("a completed tool was not marked ok: %q", out.String())
	}
	out.Reset()
	r.ToolDone("bash", operation.ResultEvent{Status: operation.StatusFailed, Error: "exit 1"})
	if !strings.Contains(out.String(), glyph("error")) {
		t.Errorf("a failed tool was not marked: %q", out.String())
	}
}

func TestToolDoneReportsAReasonNotAPayload(t *testing.T) {
	r, out := renderInto()
	r.ToolDone("read", operation.ResultEvent{Status: operation.StatusCompleted, Output: strings.Repeat("x", 9000)})
	got := out.String()
	if strings.Contains(got, "xxx") {
		t.Fatal("tool done rendered the output payload")
	}
	if !strings.Contains(got, "9000 chars") {
		t.Errorf("tool done should report a size, got %q", got)
	}
}

func TestRetryNamesTheWait(t *testing.T) {
	// A gateway backoff runs to about a minute; an unexplained minute-long pause
	// is indistinguishable from a hang, so the wait has to be on screen.
	r, out := renderInto()
	r.Retry(errors.New("upstream 503"), 2, 15*time.Second)
	got := out.String()
	if !strings.Contains(got, "2") || !strings.Contains(got, "15s") {
		t.Errorf("retry notice is missing the attempt or the wait: %q", got)
	}
}

func TestFooterReportsTokensAndCache(t *testing.T) {
	r, out := renderInto()
	r.Usage(llm.UsageInfo{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 80, ReasoningTokens: 5})
	r.Cost(llm.CostInfo{TotalUSD: 0.0123, Known: true}, "gpt-5.5")
	r.TurnEnd(nil)
	got := out.String()
	for _, want := range []string{"in 100", "out 20", "cache 80", "think 5", "$0.0123"} {
		if !strings.Contains(got, want) {
			t.Errorf("footer is missing %q:\n%s", want, got)
		}
	}
}

func TestFooterOmitsCacheWhenProviderReportsNone(t *testing.T) {
	// A provider without prompt caching reports zero, and printing a confident
	// zero reads as "caching is broken" rather than "this provider cannot".
	r, out := renderInto()
	r.Usage(llm.UsageInfo{InputTokens: 10, OutputTokens: 2})
	r.TurnEnd(nil)
	if strings.Contains(out.String(), "cache") {
		t.Errorf("footer printed a cache line for a provider that reported none:\n%s", out.String())
	}
}

func TestTurnEndMarksError(t *testing.T) {
	r, _ := renderInto()
	r.TurnEnd(errors.New("provider refused"))
	if got := r.State(); got != StateError {
		t.Fatalf("state after a failed turn = %s, want error", got)
	}
}

func TestTurnEndWithNothingProducedIsNotDone(t *testing.T) {
	// A silent turn reported as done is the one case where the status line would be
	// lying about what happened.
	r, _ := renderInto()
	r.TurnEnd(nil)
	if got := r.State(); got != StateIdle {
		t.Fatalf("state after an empty turn = %s, want idle", got)
	}
}

func TestToolStartMovesToWorkingAndBack(t *testing.T) {
	r, _ := renderInto()
	r.TurnStart()
	if got := r.State(); got != StateThinking {
		t.Fatalf("state at turn start = %s, want thinking", got)
	}
	r.ToolStart("bash", map[string]any{"command": "ls"})
	if got := r.State(); got != StateWorking {
		t.Fatalf("state during a tool = %s, want working", got)
	}
	r.ToolDone("bash", operation.ResultEvent{Status: operation.StatusCompleted})
	r.Content("all done")
	r.closeStream()
	r.Usage(llm.UsageInfo{InputTokens: 1, OutputTokens: 1})
	r.TurnEnd(nil)
	if got := r.State(); got != StateDone {
		t.Fatalf("state after a productive turn = %s, want done", got)
	}
}

func TestShortErrorStripsTheResponseBody(t *testing.T) {
	// A provider error carries the whole body, which in a status line is
	// unreadable and on some providers long enough to break the layout.
	got := ShortError(errors.New(`request failed: {"error":{"message":"a very long body that would wrap the line and ruin the layout"}}`))
	if strings.Contains(got, "{") {
		t.Errorf("ShortError kept the JSON body: %q", got)
	}
	if !strings.HasPrefix(got, "request failed:") {
		t.Errorf("ShortError lost the useful prefix: %q", got)
	}
}

func TestShortErrorBoundsTheRemainder(t *testing.T) {
	got := ShortErrorText(strings.Repeat("y", 900))
	if len(got) > 200 {
		t.Fatalf("ShortError returned %d chars, expected it bounded", len(got))
	}
}

func TestShortErrorOnNilIsEmpty(t *testing.T) {
	if got := ShortError(nil); got != "" {
		t.Fatalf("ShortError(nil) = %q, want empty", got)
	}
}

func TestToolLinesAreBounded(t *testing.T) {
	// Every line a tool produces must fit the renderer's width, or the terminal
	// wraps it and the following lines are corrupted.
	var b strings.Builder
	r2 := NewRenderer(&b, Palette{color: true})
	r2.cols = 40
	for _, args := range []map[string]any{
		{"path": strings.Repeat("p", 300)},
		{"command": strings.Repeat("c", 300)},
		{"query": strings.Repeat("q", 300)},
	} {
		r2.ToolStart("read", args)
	}
	for _, line := range strings.Split(b.String(), "\n") {
		if len(line) > 60 {
			t.Fatalf("line is %d bytes, expected it wrapped: %q", len(line), line)
		}
	}
}
