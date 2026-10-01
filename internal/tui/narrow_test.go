package tui

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// noColor strips SGR sequences so a test can assert on what a person reads.
var sgrPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

// clear-line is swapped for a NUL marker before the other escapes are stripped,
// so the replay below can still see it. NUL cannot appear in the output.
const clearLineMarker = "\x00"

func noColor(s string) string { return sgrPattern.ReplaceAllString(s, "") }

// screen replays a byte stream the way a terminal does and returns the rows that
// end up visible. Asserting on bytes proves what was written; asserting on rows
// proves what was read, and only the second is what the operator sees - an erase
// and the text it erased are both in the byte stream, in that order, and only one
// of them survives.
//
// Only the primitives this renderer emits are implemented: carriage return,
// clear-line, newline, and text written at the cursor.
func screen(out string) []string {
	out = strings.ReplaceAll(out, "\x1b[2K", clearLineMarker)
	out = sgrPattern.ReplaceAllString(out, "")
	out = strings.ReplaceAll(out, "\x1b", "")

	var (
		committed []string
		row       []rune
		col       int
	)
	commit := func() {
		committed = append(committed, strings.TrimRight(string(row), " "))
		row, col = nil, 0
	}
	for _, r := range out {
		switch {
		case r == '\r':
			col = 0
		case r == '\n':
			commit()
		case r == 0:
			row, col = nil, 0
		default:
			for len(row) <= col {
				row = append(row, ' ')
			}
			row[col] = r
			col++
		}
	}
	if len(row) > 0 {
		commit()
	}
	return committed
}

// armSpinner puts a ticker on the renderer without starting the repaint
// goroutine, so a test can fire repaint by hand and know exactly one frame was
// written. TurnStart would start a real ticker whose in-flight tick could land
// at any moment and make the assertion a race rather than a check.
func armSpinner(r *Renderer) {
	r.mu.Lock()
	r.spinner = time.NewTicker(time.Hour)
	r.mu.Unlock()
}

// The phone case: a 40-column terminal with colour on. Every line here was cut
// mid-word on screen, and in each case the cut was spent on escape bytes rather
// than on text.
func TestFitMeasuresCellsAndNotBytes(t *testing.T) {
	pal := Palette{color: true}
	for _, width := range []int{30, 40, 60} {
		for _, in := range []string{
			pal.Dim("  " + "gemini · gemini-3.5-flash-lite  ·  thinking  ·  session cli-17907956971m"),
			pal.Bold(glyph("error") + " no provider endpoint configured for model gemini-3.5-flash-lite"),
			pal.Dim("  /help for commands  " + dotRule + "  ctrl-c interrupts  " + dotRule + "  ctrl-d quits"),
		} {
			got := pal.fit(in, width)
			if w := visibleWidth(got); w > width {
				t.Errorf("fit(%d) rendered %d cells: %q", width, w, noColor(got))
			}
			over := visibleWidth(in) > width
			if over != strings.Contains(noColor(got), "…") {
				t.Errorf("fit(%d) ellipsis=%v but input was %d cells: %q",
					width, strings.Contains(noColor(got), "…"), visibleWidth(in), noColor(got))
			}
		}
	}
}

// A line that already fits is returned untouched. Fitting is a truncation
// operation and must not quietly become a rewrite.
func TestFitLeavesALineThatFitsAlone(t *testing.T) {
	pal := Palette{color: true}
	in := pal.Bold("ok") + " " + pal.Dim("read 1200 chars")
	if got := pal.fit(in, 80); got != in {
		t.Errorf("fit rewrote a line that fit:\n got %q\nwant %q", got, in)
	}
}

// Truncating a run leaves the terminal styled after the line, so the next thing
// printed inherits bold. The cut has to close what it broke.
func TestFitClosesARunItHadToBreak(t *testing.T) {
	pal := Palette{color: true}
	got := pal.fit(pal.Bold(strings.Repeat("word ", 40)), 20)
	if !strings.HasSuffix(got, ansiReset+"…") {
		t.Errorf("cut left an open run: %q", got)
	}
}

// The /help rows are pre-formatted: an indent and a padded label. Feeding those
// through a prose splitter collapsed the padding and dropped the indent, which
// is how the row came back as `hel` / `o`.
func TestWrapTextLeavesARowThatFitsUntouched(t *testing.T) {
	pal := Palette{color: true}
	row := "  " + pal.Bold("/status") + pal.Dim(strings.Repeat(" ", 3)+"active provider, model, session")
	got := wrapText(row, 80)
	if len(got) != 1 {
		t.Fatalf("a row that fits was split into %d lines: %q", len(got), got)
	}
	if got[0] != row {
		t.Errorf("preformatted row was rewritten:\n got %q\nwant %q", got[0], row)
	}
}

func TestWrapTextKeepsTheIndentWhenItMustWrap(t *testing.T) {
	got := wrapText("  /status   active provider, model, session", 20)
	if len(got) < 2 {
		t.Fatalf("expected a wrapped row, got %q", got)
	}
	if !strings.HasPrefix(got[0], "  /status   ") {
		t.Errorf("indent or padding lost on the first line: %q", got[0])
	}
	for i, l := range got {
		if len(l) > 20 {
			t.Errorf("line %d is %d cells: %q", i, len(l), l)
		}
	}
	// Continuation lines start at the margin rather than hanging under the
	// original indent, so a wrapped row does not read as a new paragraph.
	if i := strings.LastIndex(got[0], " "); i >= 0 && len(got[1]) > 0 && got[1][0] == ' ' {
		t.Errorf("continuation line kept the hanging indent: %q", got[1])
	}
}

// An escape costs no cell and is never a break opportunity. A line carrying a
// style has to measure the same as the line without it, and no line may run past
// the limit just because half of it is escape bytes.
//
// The style is deliberately not closed on every row: SGR survives a newline, so
// closing it per row and reopening on the next is the same attribute written
// four times. What matters is that the run the paragraph opened is still closed
// by the time the paragraph ends.
func TestWrapTextMeasuresStyledTextInCells(t *testing.T) {
	pal := Palette{color: true}
	para := pal.Dim(strings.Repeat("alpha beta gamma delta ", 12))
	got := wrapText(para, 24)
	if len(got) < 2 {
		t.Fatalf("expected the paragraph to wrap, got %q", got)
	}
	for i, l := range got {
		if w := visibleWidth(l); w > 24 {
			t.Errorf("line %d is %d cells: %q", i, w, noColor(l))
		}
	}
	if !strings.Contains(strings.Join(got, ""), ansiReset) {
		t.Errorf("the paragraph opened a style it never closed: %q", got[len(got)-1])
	}
}

// spinnerGlyphs is every glyph the spinner can paint, joined so a test can
// recognise a frame in a rendered row without depending on which tick was drawn.
var spinnerGlyphs = strings.Join(spinnerFrames, "")

// Reproduces the phone screen: colour off, spinner repainted in place, then a
// provider diagnostic printed. The spinner used to be left on screen with no
// newline because the erase was skipped whenever colour was off, so the two ran
// together into `  ⠸ thinkingceraa: ...`.
func TestSpinnerFrameIsErasedBeforeTheNextLine(t *testing.T) {
	for _, pal := range []Palette{Plain(), Palette{color: true}} {
		var b strings.Builder
		r := NewRenderer(&b, pal)
		r.cols = 40
		r.setState(StateThinking)
		armSpinner(r)
		r.repaint(3)
		r.stopSpinner()
		r.line("ceraa: gemini-no-endpoint model=x gemini=false items=2")

		rows := screen(b.String())
		for _, row := range rows {
			if strings.ContainsAny(row, spinnerGlyphs) && strings.Contains(row, "ceraa:") {
				t.Errorf("color=%v spinner glued onto the diagnostic: %q", pal.Color(), row)
			}
		}
		if !strings.Contains(strings.Join(rows, "\n"), "ceraa: gemini-no-endpoint") {
			t.Errorf("color=%v diagnostic missing: %q", pal.Color(), b.String())
		}
	}
}

// The answer streams with no newline of its own, so the frame has to be cleared
// before the newline that opens it or it survives as a line of its own above the
// answer.
func TestFirstContentLineClearsTheFrame(t *testing.T) {
	var b strings.Builder
	r := NewRenderer(&b, Palette{})
	r.cols = 40
	armSpinner(r)
	r.repaint(1)
	r.Content("hello")
	r.closeStream()

	joined := strings.Join(screen(b.String()), "\n")
	if !strings.Contains(joined, "hello") {
		t.Fatalf("content missing: %q", joined)
	}
	if strings.ContainsAny(joined, spinnerGlyphs) {
		t.Errorf("orphaned spinner frame left behind: %q", joined)
	}
}

// The ticker is stopped without waiting for an in-flight tick, so a repaint can
// arrive after TurnEnd. Writing then would stamp the frame over the footer.
func TestLateRepaintIsIgnoredAfterTheTurnEnds(t *testing.T) {
	var b strings.Builder
	r := NewRenderer(&b, Plain())
	r.cols = 40
	armSpinner(r)
	r.repaint(1)
	r.stopSpinner()
	before := b.Len()
	r.repaint(2)
	if b.Len() != before {
		t.Errorf("a repaint landed after the turn ended: %q", b.String())
	}
}

// The frame is clear-to-current-line. Painting it mid-answer would delete the
// answer's own line every 110ms.
func TestRepaintDoesNotPaintWhileStreaming(t *testing.T) {
	var b strings.Builder
	r := NewRenderer(&b, Plain())
	r.cols = 40
	armSpinner(r)
	r.mu.Lock()
	r.streaming = true
	r.mu.Unlock()
	r.repaint(1)
	if b.Len() != 0 {
		t.Errorf("repaint wrote over a streamed answer: %q", b.String())
	}
	r.stopSpinner()
}

// The status line degrades by dropping fields, never by cutting through one. An
// elided `gpt-5.6-lun…` names no model at all, which is worse than showing none.
func TestStatusLineDropsFieldsInsteadOfBeingCut(t *testing.T) {
	pal := Palette{color: true}
	for _, width := range []int{20, 24, 40, 80} {
		got := pal.statusLine(width, "thinking", "gemini-3.5-flash-lite", "low")
		if w := visibleWidth(got); w > width {
			t.Errorf("width %d: status line is %d cells: %q", width, w, noColor(got))
		}
		if strings.Contains(noColor(got), "…") {
			t.Errorf("width %d: status line was cut through a field: %q", width, noColor(got))
		}
		// The state is what the run is doing; it is the last thing to go.
		if !strings.Contains(noColor(got), "thinking") {
			t.Errorf("width %d: status line lost the state: %q", width, noColor(got))
		}
	}
	// Effort is the first to be dropped, and the model before the state.
	if got := noColor(pal.statusLine(80, "thinking", "gpt-5.6-luna", "low")); !strings.Contains(got, "low") {
		t.Errorf("a wide status line dropped the effort: %q", got)
	}
}

func TestHintBarNamesNoTruncatedKey(t *testing.T) {
	pal := Palette{color: true}
	for _, width := range []int{24, 30, 40, 80} {
		got := pal.hintBar(width)
		if w := visibleWidth(got); w > width {
			t.Errorf("width %d: hint bar is %d cells: %q", width, w, noColor(got))
		}
		if strings.Contains(noColor(got), "…") {
			t.Errorf("width %d: a hint was cut through its key: %q", width, noColor(got))
		}
		if !strings.Contains(noColor(got), "/help") {
			t.Errorf("width %d: the one hint that always earns its place is gone: %q", width, noColor(got))
		}
	}
}

// Every failure worth acting on ends in a remedy. Fitting the message dropped
// that remedy exactly when it was the only line saying what to do next.
func TestErrorKeepsTheRemedyOnANarrowTerminal(t *testing.T) {
	var b strings.Builder
	r := NewRenderer(&b, Plain())
	r.cols = 40
	r.Error(errors.New(`no provider endpoint configured for model "gemini-3.5-flash-lite": set a base URL via provider onboarding or CERAA_API_URL`))

	out := b.String()
	if strings.Contains(out, "…") {
		t.Errorf("the error was truncated: %q", out)
	}
	if !strings.Contains(out, "CERAA_API_URL") {
		t.Errorf("the remedy was cut off: %q", out)
	}
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if len(l) > 40 {
			t.Errorf("error line is %d cells: %q", len(l), l)
		}
	}
}

// A destination that is not a terminal has no window to resize, so a width a test
// or a captured log pins has to stick: r.width() re-reads the terminal size, and
// re-reading it for a buffer would silently undo the pin.
func TestWidthKeepsAPinnedWidthForANonTerminalDestination(t *testing.T) {
	r := NewRenderer(&strings.Builder{}, Plain())
	r.cols = 40
	if got := r.width(); got != 40 {
		t.Errorf("width() = %d, want the pinned 40", got)
	}
}

// summariseRun degrades the same way the status line does: the two numbers that
// answer "how big was this turn" are the last to go.
func TestRunSummaryDropsTailSegmentsBeforeItCuts(t *testing.T) {
	pal := Palette{color: true}
	for _, width := range []int{20, 30, 40, 80} {
		got := summariseRun(width, pal, "1234", "567", "890", "12", "$0.0123", "")
		if w := visibleWidth(got); w > width {
			t.Errorf("width %d: summary is %d cells: %q", width, w, noColor(got))
		}
		if strings.Contains(noColor(got), "…") {
			t.Errorf("width %d: summary was cut: %q", width, noColor(got))
		}
	}
	if w := summariseRun(40, pal, "1234", "567", "890", "12", "$0.0123", ""); !strings.Contains(w, "1234") || !strings.Contains(w, "567") {
		t.Errorf("the in/out pair did not survive at width 40: %q", noColor(w))
	}
}
