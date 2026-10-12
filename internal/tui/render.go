package tui

import (
	"fmt"
	"io"
	"os"
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
	usage      llm.UsageInfo
	usageTrust llm.UsageTrust
	cost       llm.CostInfo
	// costModel is the model the cost figure belongs to, which is not always the
	// active one after a mid-run switch.
	costModel string

	// live counts tools started against tools finished, so the working line can
	// say "3 running" rather than only that something is.
	live int

	// bufMu guards the answer buffer below it, and is deliberately not mu.
	//
	// mu is held across the terminal writes in write and draw, and flushing the
	// buffer has to write what it flushed, so one lock for both would have to be
	// re-entered and Go's is not reentrant. The buffer needs a lock at all
	// because it is reached from two goroutines: Content streams deltas from the
	// provider while a tool line from the coordinator arrives and closes the
	// stream, and closeStream used to read aTail while Content was rewriting it.
	// That torn string header is what panicked inside strings.TrimSpace.
	//
	// The order is always bufMu then mu, and nothing takes mu and then bufMu, so
	// holding this across the writes below cannot deadlock.
	bufMu sync.Mutex

	// Answer text is parked until it can be told apart from a Markdown table.
	// The answer arrives a delta at a time and a table can only be laid out once
	// its header and rule have been seen, so pipe-led lines are held. Everything
	// else is written straight through, so prose still streams at line speed and
	// these are empty almost all of the time.
	aTail  string
	aLine  string
	aCells []string
	aTable *tableBlock
	// aFence is true between a pair of ``` lines. Everything inside is literal,
	// so it is written through without wrapping and without looking for markers.
	// Code is full of asterisks and underscores and scanning it would mangle it.
	aFence bool

	// painted records that the repaint ticker has left a status line on screen
	// with no newline after it. Erasing is then conditional on having painted,
	// so a plain log does not fill up with carriage returns.
	painted bool

	// saidSomething records that the model produced text, because a turn can be
	// productive and still report no tokens on a provider that does not count them.
	saidSomething bool

	// now is injectable so tests do not depend on the wall clock.
	now func() time.Time
}

// NewRenderer builds a renderer writing to out.
//
// A destination that is not a terminal has no window size, so it gets a fixed
// width and the same code path serves a captured log. A terminal does have a
// size, and colour being off must not freeze it at a guess: laying a 40-column
// phone out at 100 columns is what let the terminal hard-wrap our own rows
// mid-word, which is how `/help` came back as `hel` / `o`.
func NewRenderer(out io.Writer, pal Palette) *Renderer {
	r := &Renderer{out: out, pal: pal, cols: width(), state: StateIdle, now: time.Now}
	if !term.isTerminal(os.Stdout.Fd()) {
		r.cols = 100
	}
	return r
}

// width is the current terminal width, re-read on every use.
//
// It is re-read rather than cached because a terminal is resized: a phone
// rotates, a split is resized, a window is maximised. A width captured at
// startup and trusted forever lays out lines the terminal has already stopped
// being wide enough for.
//
// The refresh is gated on the destination being a real terminal, so a captured
// log keeps the fixed width it was given and a test that pins a width keeps it.
func (r *Renderer) width() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.out.(*os.File); ok && term.isTerminal(os.Stdout.Fd()) && term.isTerminal(f.Fd()) {
		if w := width(); w >= 20 {
			r.cols = w
		}
	}
	return r.cols
}

// Out exposes the destination so the loop can read and write the same stream.
func (r *Renderer) Out() io.Writer { return r.out }

// State reports what the current turn is doing.
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

// write emits s as one write, under the renderer's lock.
//
// Holding the lock across the write is what keeps concurrent tool lines from
// interleaving inside a line of their own: the coordinator dispatches tools from
// several goroutines and the repaint ticker writes from another, so without it
// two callbacks can land in the same row. The lock is never taken by a caller
// that already holds it, so a repaint cannot deadlock here.
func (r *Renderer) write(s string) {
	if s == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprint(r.out, s)
}

// live writes a block of pre-laid-out lines, each already fitted to the width.
//
// A block is passed rather than a callback because a caller assembling a table
// needs the rendered lines to be written together: interleaving them with a
// concurrent tool line would break the alignment the table exists to provide.
func (r *Renderer) writeBlock(_ string, lines []string) {
	r.closeStream()
	r.draw(func() {
		for _, l := range lines {
			fmt.Fprint(r.out, l+"\n")
		}
	})
}

// line emits s followed by a newline, wrapped to the terminal width.
func (r *Renderer) line(s string) {
	// The answer, if one is still open, has to be closed before our own line or
	// ours lands at the end of its last row.
	r.closeStream()
	rows := wrapText(s, r.width())
	r.draw(func() {
		for _, l := range rows {
			fmt.Fprint(r.out, l+"\n")
		}
	})
}

// TurnStart marks the beginning of a turn and starts the spinner.
func (r *Renderer) TurnStart() {
	r.setState(StateThinking)
	r.bufMu.Lock()
	r.aTail, r.aLine, r.aCells, r.aTable = "", "", nil, nil
	r.aFence = false
	r.bufMu.Unlock()
	r.mu.Lock()
	r.streaming = false
	r.live = 0
	r.saidSomething = false
	r.usage = llm.UsageInfo{}
	r.usageTrust = llm.UsageTrust{Trusted: true}
	r.cost = llm.CostInfo{}
	r.mu.Unlock()
	r.startSpinner()
}

// TurnEnd stops the spinner, prints the footer, and settles the state line.
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
	// that returns an empty body with a 200 leaves the status line claiming success.
	said := r.usage.TotalTokens > 0 || r.saidSomething
	if err != nil {
		state = StateError
	} else if !said {
		state = StateIdle
	}
	r.state = state
	usage, trust, cost, costModel := r.usage, r.usageTrust, r.cost, r.costModel
	r.mu.Unlock()
	r.footer(usage, trust, cost, costModel, err)
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
		// The frame the ticker left on the current line has to be cleared before
		// the newline, or it stays behind as a dangling line of its own. Erasing
		// first is what keeps the answer from starting under a status frame.
		//
		// Normal weight, deliberately. This used to open the whole answer in bold,
		// which was a way of making the answer stand out from the chrome before
		// there was any inline emphasis to render. Now that `**bold**` and headings
		// are real, a body that is entirely bold leaves nothing for them to be
		// bolder than, and a wall of bold is harder to read than plain text.
		r.draw(func() { fmt.Fprint(r.out, "\n") })
	}
	for _, piece := range r.answer(s) {
		r.write(piece)
	}
}

// answer pushes streamed answer text through the table detector.
//
// The answer arrives a delta at a time and a table can only be laid out once its
// header and rule have been seen, so something has to be held. Only a line that
// could become a table row is: a row has to begin with a pipe, so a line that does
// not is written through the moment it arrives, and the answer still starts
// moving on the first token.
//
// The unit is a line, and it has to be. Everything the renderer knows how to do
// to answer text needs a whole line: emphasis markers close somewhere after they
// open, a bullet only reads as a bullet with its continuation indented under it,
// and a row is only known to be a row once its rule has arrived. Writing each
// delta straight through meant none of that ever ran, because a delta usually
// has no newline in it at all.
//
// So the tail of the answer is held until its line is complete. What that costs
// is that a partial line waits, bounded by however long the model takes to reach
// its next newline, which for prose is a sentence rather than an answer. What it
// buys is that the answer arrives as something a person can read.
func (r *Renderer) answer(s string) []string {
	r.bufMu.Lock()
	defer r.bufMu.Unlock()
	width := r.width()
	r.aTail += s
	var out []string
	for {
		i := strings.IndexByte(r.aTail, '\n')
		if i < 0 {
			break
		}
		line := r.aTail[:i]
		r.aTail = r.aTail[i+1:]
		if looksLikeRowStart(line) {
			out = append(out, r.answerLine(line, width)...)
			continue
		}
		// A line that is not a row ends any table in progress. This has to be
		// settled here rather than inside answerLine, because answerLine is only
		// reached by pipe-led lines: a blank line after the last row is what closes
		// a table, and it never gets that far, so the table was held until the
		// answer ended and printed after the prose that followed it.
		if r.aTable != nil {
			finished := r.aTable
			r.aTable = nil
			out = append(out, terminate(renderTable(r.pal, finished, width))...)
		}
		// Prose. A held table header that never got its rule is released first, so
		// it keeps its place in the answer rather than jumping to the next line.
		out = append(out, r.releaseHeld(width)...)
		out = append(out, r.answerProse(line, width)...)
	}
	return out
}

// answerProse renders one line that is not part of a table, honouring a code
// fence if one is open.
func (r *Renderer) answerProse(line string, width int) []string {
	trimmed := strings.TrimSpace(line)
	if r.aFence {
		// Only a fence line closes a fence. Toggling on every line inside one meant
		// the second line of the block turned it off, so its indentation was
		// reflowed and everything after it was treated as literal.
		if strings.HasPrefix(trimmed, "```") {
			r.aFence = false
			return []string{line + "\n"}
		}
		return wrapFenceLine(line, width)
	}
	if strings.HasPrefix(trimmed, "```") {
		r.aFence = true
		// Verbatim, and unwrapped: a fence is a promise about what is on the line,
		// and reflowing the inside of one breaks the indentation it exists to keep.
		return []string{line + "\n"}
	}
	return r.pal.answerBlock(line, width)
}

// looksLikeRowStart reports whether a line could still turn into a table row.
//
// Only the leading pipe is checked, and only the first character that is not
// whitespace. A partial line is judged on what it has so far, which is enough:
// a row cannot begin with anything else.
func looksLikeRowStart(s string) bool {
	return strings.HasPrefix(strings.TrimLeft(s, " "), "|")
}

// answerLine decides what one complete pipe-led line of answer text is: a table
// row to hold, or the end of a table.
func (r *Renderer) answerLine(line string, width int) []string {
	if r.aTable != nil {
		if cells, ok := parseTableRow(line); ok {
			r.aTable.rows = append(r.aTable.rows, cells)
			return nil
		}
		// Not a row, so the table ended here. Render it, then judge this line.
		finished := r.aTable
		r.aTable = nil
		return append(terminate(renderTable(r.pal, finished, width)), r.answerLine(line, width)...)
	}

	cells, isRow := parseTableRow(line)
	if !isRow {
		// A pipe-led line that is not a row after all: prose that mentions a pipe.
		return append(r.releaseHeld(width), wrapLines(line, width)...)
	}
	// A rule straight after a held header is what makes this a table rather than
	// a sentence that happens to contain a pipe.
	if r.aCells != nil && isTableRule(cells) && len(cells) == len(r.aCells) {
		r.aTable = &tableBlock{header: r.aCells}
		r.aCells = nil
		r.aLine = ""
		return nil
	}
	// Hold it. One more line decides, and guessing wrong costs a line of delay
	// rather than a mangled table.
	out := r.releaseHeld(width)
	r.aLine, r.aCells = line, cells
	return out
}

// releaseHeld writes out a held header that turned out not to be one.
func (r *Renderer) releaseHeld(width int) []string {
	if r.aLine == "" {
		return nil
	}
	line := r.aLine
	r.aLine, r.aCells = "", nil
	return r.answerProse(line, width)
}

// flushAnswer empties the detector, so a table that ran to the end of the answer
// is still laid out rather than lost.
//
// The lock is held across the writes as well as the reads: content computed
// after the tail would otherwise be written before it, and the answer would
// come out in the wrong order.
func (r *Renderer) flushAnswer() {
	r.bufMu.Lock()
	defer r.bufMu.Unlock()
	if r.aTail == "" && r.aLine == "" && r.aTable == nil {
		return
	}
	width := r.width()
	var out []string
	if r.aTable != nil {
		out = append(out, terminate(renderTable(r.pal, r.aTable, width))...)
		r.aTable = nil
	}
	out = append(out, r.releaseHeld(width)...)
	if r.aTail != "" {
		out = append(out, r.answerProse(r.aTail, width)...)
		r.aTail = ""
	}
	for _, piece := range out {
		r.write(piece)
	}
}

// terminate ends every row a table rendered.
//
// The table builders return rows without a newline, because they are also used
// by tests that read them as a list. A piece the renderer writes has to carry its
// own newline, and the one that forgot did not: the whole table went out on a
// single line, which the terminal then cut wherever it liked.
func terminate(rows []string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r+"\n")
	}
	return out
}

// wrapLines wraps one line and terminates every row it produced.
func wrapLines(line string, width int) []string {
	rows := wrapText(line, width)
	out := make([]string, 0, len(rows))
	for _, l := range rows {
		out = append(out, l+"\n")
	}
	return out
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
		r.draw(func() { fmt.Fprint(r.out, "\n"+r.pal.DimOn()) })
	}
	r.write(r.pal.Dim(s))
}

// closeStream ends an open answer or reasoning run.
//
// Without this a tool line would be printed on the same row as the tail of an
// answer, and the next answer would continue in bold from the wrong place.
func (r *Renderer) closeStream() {
	// Before the reset, so a table that ran to the end of the answer is laid out
	// as part of the answer rather than after the style that framed it.
	r.flushAnswer()
	r.mu.Lock()
	wasStreaming := r.streaming
	r.streaming = false
	r.mu.Unlock()
	if !wasStreaming {
		return
	}
	// One write for the reset and the newline, so a repaint cannot be observed
	// between them with the style already reset and the newline not yet sent.
	r.draw(func() { fmt.Fprint(r.out, r.pal.Reset()+"\n") })
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
	r.mu.Lock()
	if r.live > 0 {
		r.live--
	}
	r.mu.Unlock()
	status := "ok"
	if res.Status != operation.StatusCompleted || res.Error != "" {
		status = "error"
	}
	detail := ToolResultSummary(res)
	line := formatToolLine(status, ToolLabel(name), detail)
	if status == "error" {
		line = r.pal.Bold(line)
	} else {
		line = r.pal.Dim(line)
	}
	// Fitted rather than wrapped: a tool line is already an aligned single row,
	// and breaking it mid-detail would lose the detail it exists to carry while
	// leaving no alignment to preserve.
	r.line(r.pal.fit(line, r.width()))
}

// Retry reports an upstream retry, naming the wait.
//
// A gateway backoff runs to about a minute and an unexplained minute-long pause
// is indistinguishable from a hang, so the wait is stated rather than logged.
func (r *Renderer) Retry(err error, attempt int, delay time.Duration) {
	r.closeStream()
	msg := fmt.Sprintf("retry %d in %s", attempt, delay.Round(time.Second))
	if err != nil {
		msg += ": " + ShortError(err)
	}
	// Not fitted: the wait is the important half and the cause is the other half,
	// and truncating mid-cause leaves a retry with no reason. Wrapping keeps both.
	r.line(r.pal.Dim(glyph("warn") + " " + msg))
}

// Compaction reports context compaction starting and finishing.
func (r *Renderer) Compaction(done bool, err error) {
	r.closeStream()
	if err != nil {
		r.line(r.pal.Dim(glyph("error") + " compaction skipped: " + ShortError(err)))
		return
	}
	verb := "compacting context"
	if done {
		verb = "context compacted"
	}
	r.line(r.pal.Dim(glyph("info") + " " + verb))
}

// Usage records provider token counts for the footer, and whether they may be
// believed.
//
// The two travel together because they are one fact. A renderer handed only the
// number has no way to leave it out, and the whole point of reconciling the
// provider's count against our own is that a figure we have already called a
// running total must not then be printed as this turn's input.
func (r *Renderer) Usage(u llm.UsageInfo, trust llm.UsageTrust) {
	r.mu.Lock()
	r.usage = u
	r.usageTrust = trust
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
//
// The message is wrapped rather than fitted. Every failure worth acting on ends
// in a remedy - which variable to set, which command to run - and fitting it to
// the terminal dropped that remedy on the floor exactly when it was the only
// line that told the operator what to do next.
func (r *Renderer) Error(err error) {
	r.setState(StateError)
	r.closeStream()
	if err != nil {
		r.line(r.pal.Bold(glyph("error") + " " + ShortError(err)))
	}
}

// footer prints the per-turn summary: tokens, cost, and cache reads.
//
// Cache is only printed when the provider reported it, because a provider that
// does not support prompt caching reports zero and printing a confident 0% would
// read as "caching is not working" rather than "this provider cannot".
func (r *Renderer) footer(usage llm.UsageInfo, trust llm.UsageTrust, cost llm.CostInfo, costModel string, runErr error) {
	in, out, cache, reason := "", "", "", ""
	// Input is printed only when the count describes this turn. Cost is already
	// withheld the same way and cache falls back to silence, so a third figure
	// printed from a count we have rejected would be the odd one out.
	if usage.InputTokens > 0 && trust.Trusted {
		in = strconv.Itoa(usage.InputTokens)
	}
	if usage.OutputTokens > 0 {
		out = strconv.Itoa(usage.OutputTokens)
	}
	// Cache is only printed when the provider reported it: a provider without
	// prompt caching reports zero, and a confident "cache 0" reads as "caching is
	// broken" rather than "this provider cannot".
	if usage.CachedInputTokens > 0 {
		cache = strconv.Itoa(usage.CachedInputTokens)
	}
	if usage.ReasoningTokens > 0 {
		reason = strconv.Itoa(usage.ReasoningTokens)
	}
	money := ""
	if cost.TotalUSD > 0 {
		money = "$" + strconv.FormatFloat(cost.TotalUSD, 'f', 4, 64)
		if costModel != "" {
			money += " " + costModel
		}
	}
	w := r.width()
	line := summariseRun(w, r.pal, in, out, cache, reason, money, "")
	// The draw runs even when there is nothing to summarise, because the ticker
	// may have left a frame on the current line and TurnEnd is the last word in
	// the turn: leaving it would park a status frame under the next prompt.
	r.draw(func() {
		if line == "" {
			return
		}
		fmt.Fprint(r.out, r.pal.rule(w)+"\n")
		for _, l := range wrapText(line, w) {
			fmt.Fprint(r.out, l+"\n")
		}
	})
}

// startSpinner begins repainting the status line.
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
//
// The repaint ticker is stopped without waiting for an in-flight tick, so a tick
// can already be running when the turn ends; checking the spinner under the same
// lock that performs the write is what stops a late frame from being stamped over
// the footer.
//
// It also does not paint while an answer is streaming. The erase is
// clear-to-current-line, so painting mid-answer would delete the answer's own
// line every 110ms and replace it with the frame.
func (r *Renderer) repaint(tick int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.spinner == nil || r.streaming {
		return
	}
	label := r.state.String()
	if r.live > 0 {
		label = fmt.Sprintf("%s %d", r.state.String(), r.live)
	}
	line := fmt.Sprintf("  %s %s", Spinner(tick), label)
	// Carriage return plus clear-to-end, never cursor-up: a cursor-up on a line
	// that has already scrolled walks the cursor into committed output.
	fmt.Fprint(r.out, "\r\x1b[2K"+r.pal.Dim(line))
	r.painted = true
}

// draw runs emit with the renderer lock held, first clearing whatever frame the
// repaint ticker left on the current line.
//
// The erase and the writes that follow it are deliberately in one critical
// section. Split them and the ticker lands in the gap, repainting the frame onto
// the line that was just cleared, so the output it was making room for prints
// underneath it. That is exactly how a spinner and a provider diagnostic came to
// share one row.
//
// Carrying an in-place erase also fixes the plain-mode case: the erase used to
// be skipped when colour was off, as though clearing were part of the styling,
// while the repaint itself wrote the frame regardless. The frame was then left
// on screen with no newline, and the next write ran straight into it.
func (r *Renderer) draw(emit func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.painted {
		fmt.Fprint(r.out, "\r\x1b[2K")
		r.painted = false
	}
	emit()
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
	case "read", "write", "edit_line", "batch_edit_file", "move", "copy", "delete":
		return "path"
	case "bash", "run_command_with_secret":
		return "command"
	case "webfetch":
		return "url"
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
