package tui

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/operation"
)

// Renderer turns coordinator callbacks into terminal output.
//
// It is the only place that knows what a run looks like on screen, and it is
// deliberately not the thing that decides what a run does. Every method is safe
// for concurrent use because the coordinator dispatches tools concurrently and
// several callbacks arrive from different goroutines while the spinner is
// repainting, so a shared cursor column or a running token total is a data race
// unless it is guarded here.
type Renderer struct {
	mu    sync.Mutex
	out   io.Writer
	pal   Palette
	cols  int
	state State

	// streaming tracks whether an answer is mid-write, so a tool line arriving
	// between two content deltas closes the paragraph first instead of landing
	// inside it.
	streaming bool
	// spinner is the repaint ticker, running only while a turn is active so an
	// idle prompt costs nothing.
	spinner   *time.Ticker
	spinnerID int
	// ticks counts spinner frames.
	ticks int

	// totals accumulate across a run for the footer.
	usage llm.UsageInfo
	cost  llm.CostInfo
	// costModel is the model the cost figure belongs to, which is not always the
	// active one after a mid-run switch.
	costModel string

	// live counts tools started against tools finished, so the working line can
	// say "3 running" rather than only that something is.
	live int

	// saidSomething records that the model produced text, because a turn can be
	// productive and still report no tokens on a provider that does not count them.
	saidSomething bool

	// now is injectable so tests do not depend on the wall clock.
	now func() time.Time
}

// NewRenderer builds a renderer writing to out.
//
// A non-terminal destination gets a plain palette and a fixed width, so the same
// code path serves an interactive run and a captured log.
func NewRenderer(out io.Writer, pal Palette) *Renderer {
	r := &Renderer{out: out, pal: pal, cols: width(), state: StateIdle, now: time.Now}
	if !pal.Color() {
		r.cols = 100
	}
	return r
}

// Out exposes the destination so the loop can read and write the same stream.
func (r *Renderer) Out() io.Writer { return r.out }

// State reports the current mascot state.
func (r *Renderer) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *Renderer) setState(s State) {
	r.mu.Lock()
	if r.state != s {
		r.state = s
	}
	r.mu.Unlock()
}

// write emits s as one write, so concurrent tool lines never interleave inside
// a line of their own.
func (r *Renderer) write(s string) {
	if s == "" {
		return
	}
	fmt.Fprint(r.out, s)
}

// line emits s followed by a newline, wrapped to the terminal width.
func (r *Renderer) line(s string) {
	for _, l := range wrapText(s, r.cols) {
		r.write(l + "\n")
	}
}

// rule is the horizontal separator, drawn with a glyph that is one cell wide in
// every terminal rather than a box-drawing character that some fonts render
// double width.
func (r *Renderer) rule() {
	n := r.cols
	if n > 72 {
		n = 72
	}
	if n < 8 {
		n = 8
	}
	r.write(r.pal.Dim(strings.Repeat("-", n)) + "\n")
}

// TurnStart marks the beginning of a turn and starts the spinner.
func (r *Renderer) TurnStart() {
	r.setState(StateThinking)
	r.mu.Lock()
	r.streaming = false
	r.live = 0
	r.saidSomething = false
	r.usage = llm.UsageInfo{}
	r.cost = llm.CostInfo{}
	r.mu.Unlock()
	r.startSpinner()
}

// TurnEnd stops the spinner, prints the footer, and settles the mascot.
//
// A nil error sets the done state and any error sets the error state, and a turn
// that produced nothing is not reported as done, because a silent turn is the
// one case where "done" would be a lie.
func (r *Renderer) TurnEnd(err error) {
	r.stopSpinner()
	r.closeStream()
	r.mu.Lock()
	state := StateDone
	// A turn that said nothing is not a turn that worked. Without this a provider
	// that returns an empty body with a 200 leaves the mascot claiming success.
	said := r.usage.TotalTokens > 0 || r.saidSomething
	if err != nil {
		state = StateError
	} else if !said {
		state = StateIdle
	}
	r.state = state
	usage, cost, costModel := r.usage, r.cost, r.costModel
	r.mu.Unlock()
	r.footer(usage, cost, costModel, err)
}

// Content streams assistant text.
//
// Text arrives in deltas, so it is written straight through with no buffering:
// buffering would either delay the first token or need a flush heuristic, and
// the whole point of streaming is that the answer starts moving immediately.
func (r *Renderer) Content(s string) {
	if s == "" {
		return
	}
	r.mu.Lock()
	first := !r.streaming
	r.streaming = true
	r.saidSomething = true
	r.mu.Unlock()
	if first {
		r.write("\n" + r.pal.BoldOn())
	}
	r.write(s)
}

// Reasoning streams model thinking.
//
// Thinking is dim and prefixed once, so it is visibly not the answer. It is
// written rather than suppressed because a model that says it is looking at
// something is doing less confidently than one that shows the reasoning, and the
// operator can see the difference.
func (r *Renderer) Reasoning(s string) {
	if s == "" {
		return
	}
	r.mu.Lock()
	first := !r.streaming
	r.streaming = true
	r.saidSomething = true
	r.mu.Unlock()
	if first {
		r.write("\n" + r.pal.DimOn())
	}
	r.write(r.pal.Dim(s))
}

// closeStream ends an open answer or reasoning run.
//
// Without this a tool line would be printed on the same row as the tail of an
// answer, and the next answer would continue in bold from the wrong place.
func (r *Renderer) closeStream() {
	r.mu.Lock()
	wasStreaming := r.streaming
	r.streaming = false
	r.mu.Unlock()
	if wasStreaming {
		r.write(r.pal.Reset())
		r.write("\n")
	}
}

// ToolStart reports a tool beginning.
//
// The argument summary is bounded and never rendered with %v, because arguments
// arrive as a map and a content field would otherwise dump a whole file into the
// transcript.
func (r *Renderer) ToolStart(name string, args map[string]any) {
	r.closeStream()
	r.setState(StateWorking)
	r.mu.Lock()
	r.live++
	r.mu.Unlock()
	r.line(r.pal.Dim(Spinner(0)) + " " + r.pal.Bold(ToolLabel(name)) + " " + r.pal.Dim(ToolArgsSummary(name, args)))
}

// ToolDone reports a tool finishing.
//
// A failed tool is marked rather than colour-coded, and a tool that failed
// because the model called it wrongly is called out separately from one that
// failed on the machine, because the first is the model's to fix and the second
// is not.
func (r *Renderer) ToolDone(name string, res operation.ResultEvent) {
	r.stopSpinnerLine()
	r.mu.Lock()
	if r.live > 0 {
		r.live--
	}
	r.mu.Unlock()
	mark := "[ok]"
	detail := ToolResultSummary(res)
	if res.Status != operation.StatusCompleted {
		mark = "[!!]"
	}
	if res.Error != "" {
		mark = "[!!]"
	}
	r.line(r.pal.Dim(mark) + " " + ToolLabel(name) + " " + r.pal.Dim(detail))
}

// Retry reports an upstream retry, naming the wait.
//
// A gateway backoff runs to about a minute and an unexplained minute-long pause
// is indistinguishable from a hang, so the wait is stated rather than logged.
func (r *Renderer) Retry(err error, attempt int, delay time.Duration) {
	r.closeStream()
	msg := fmt.Sprintf("upstream error, retry %d in %s", attempt, delay.Round(time.Second))
	if err != nil {
		msg += ": " + ShortError(err)
	}
	r.line(r.pal.Dim("[..] " + msg))
}

// Compaction reports context compaction starting and finishing.
func (r *Renderer) Compaction(done bool, err error) {
	r.closeStream()
	if err != nil {
		r.line(r.pal.Dim("[!!] compaction skipped: " + ShortError(err)))
		return
	}
	verb := "compacting context"
	if done {
		verb = "context compacted"
	}
	r.line(r.pal.Dim("[..] " + verb))
}

// Usage records provider token counts for the footer.
func (r *Renderer) Usage(u llm.UsageInfo) {
	r.mu.Lock()
	r.usage = u
	r.mu.Unlock()
}

// Cost records the cost figure and the model it belongs to.
func (r *Renderer) Cost(c llm.CostInfo, model string) {
	r.mu.Lock()
	r.cost = c
	r.costModel = model
	r.mu.Unlock()
}

// Error reports a run-level failure.
func (r *Renderer) Error(err error) {
	r.setState(StateError)
	r.closeStream()
	if err != nil {
		r.line(r.pal.Bold("[!!] ") + ShortError(err))
	}
}

// footer prints the per-turn summary: tokens, cost, and cache reads.
//
// Cache is only printed when the provider reported it, because a provider that
// does not support prompt caching reports zero and printing a confident 0% would
// read as "caching is not working" rather than "this provider cannot".
func (r *Renderer) footer(usage llm.UsageInfo, cost llm.CostInfo, costModel string, runErr error) {
	parts := []string{}
	if usage.InputTokens > 0 {
		parts = append(parts, "in "+strconv.Itoa(usage.InputTokens))
	}
	if usage.OutputTokens > 0 {
		parts = append(parts, "out "+strconv.Itoa(usage.OutputTokens))
	}
	if usage.CachedInputTokens > 0 {
		parts = append(parts, "cache "+strconv.Itoa(usage.CachedInputTokens))
	}
	if usage.ReasoningTokens > 0 {
		parts = append(parts, "think "+strconv.Itoa(usage.ReasoningTokens))
	}
	if cost.TotalUSD > 0 {
		amount := "$" + strconv.FormatFloat(cost.TotalUSD, 'f', 4, 64)
		if costModel != "" {
			amount += " " + costModel
		}
		parts = append(parts, amount)
	}
	if len(parts) == 0 && runErr == nil {
		return
	}
	r.rule()
	if len(parts) > 0 {
		r.line(r.pal.Dim("  " + strings.Join(parts, "  ")))
	}
}

// startSpinner begins repainting the mascot's state line.
func (r *Renderer) startSpinner() {
	r.mu.Lock()
	if r.spinner != nil {
		r.mu.Unlock()
		return
	}
	r.ticks = 0
	r.spinner = time.NewTicker(110 * time.Millisecond)
	t := r.spinner
	r.mu.Unlock()
	go func() {
		for range t.C {
			r.mu.Lock()
			r.ticks++
			tick := r.ticks
			r.mu.Unlock()
			r.repaint(tick)
		}
	}()
}

func (r *Renderer) stopSpinner() {
	r.mu.Lock()
	if r.spinner != nil {
		r.spinner.Stop()
		r.spinner = nil
	}
	r.mu.Unlock()
}

// repaint redraws the state line in place.
func (r *Renderer) repaint(tick int) {
	r.mu.Lock()
	state, live := r.state, r.live
	r.mu.Unlock()
	label := state.String()
	if live > 0 {
		label = fmt.Sprintf("%s %d", state.String(), live)
	}
	line := fmt.Sprintf("  %s %s", Spinner(tick), label)
	// Erase the previous line before writing. Carriage return plus a clear-to-end
	// is used rather than a cursor-up, so a repaint that follows a scrolled line
	// cannot walk the cursor up into output that is already committed.
	r.write("\r\x1b[2K" + r.pal.Dim(line))
}

// stopSpinnerLine erases the spinner line so a tool result is not printed under
// it.
func (r *Renderer) stopSpinnerLine() {
	if !r.pal.Color() {
		return
	}
	r.write("\r\x1b[2K")
}

// ToolLabel gives a tool a name a person recognises, leaving unknown tools as
// they arrived rather than guessing.
func ToolLabel(name string) string { return name }

// ToolArgsSummary renders the argument that identifies a call, bounded.
//
// The bound is not cosmetic: a `content` argument is an entire file, and
// rendering it verbatim puts the file in the transcript and the scrollback.
// Anything that is not a short identifying string is reported as a size instead.
func ToolArgsSummary(name string, args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	key := argKeyFor(name)
	raw, ok := args[key]
	if !ok {
		for _, k := range []string{"path", "file", "command", "query", "url", "name", "pattern"} {
			if v, found := args[k]; found {
				raw, ok = v, true
				break
			}
		}
		if !ok {
			return fmt.Sprintf("(%d args)", len(args))
		}
	}
	s := fmt.Sprint(raw)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	const limit = 90
	if len(s) > limit {
		s = s[:limit] + "..."
	}
	return s
}

// argKeyFor names the identifying argument per tool, so the summary shows the
// path for a read and the command for a bash rather than whichever key happened
// to be first in map order, which is random in Go.
func argKeyFor(name string) string {
	switch name {
	case "read", "write", "edit_line", "batch_edit_file", "view_image", "move", "copy", "delete":
		return "path"
	case "bash", "run_command_with_secret":
		return "command"
	case "webfetch":
		return "url"
	case "fts_search":
		return "query"
	default:
		return "path"
	}
}

// ToolResultSummary renders a tool outcome in a few characters.
//
// The output length is reported rather than the output, because the output is
// already in the transcript from the model's perspective and repeating it here
// only doubles what the operator scrolls past.
func ToolResultSummary(res operation.ResultEvent) string {
	if res.Error != "" {
		return ShortErrorText(res.Error)
	}
	if n := len(res.Output); n > 0 {
		return strconv.Itoa(n) + " chars"
	}
	return ""
}

// ShortError reduces an error to one readable line.
//
// A provider error carries the whole response body, which in a status line is
// unreadable and on some providers long enough to break the layout, so anything
// from the first brace onward is dropped.
func ShortError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if i := strings.IndexByte(msg, '{'); i > 0 {
		msg = msg[:i]
	}
	msg = strings.TrimSpace(strings.ReplaceAll(msg, "\n", " "))
	const limit = 160
	if len(msg) > limit {
		msg = msg[:limit] + "..."
	}
	return msg
}

// ShortErrorText is ShortError for a plain string.
func ShortErrorText(s string) string { return ShortError(fmt.Errorf("%s", s)) }
