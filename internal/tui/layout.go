package tui

import (
	"fmt"
	"strings"
)

// A vertical key/value block, aligned on a fixed gutter.
//
// Aster's status card and its /status output both use the same grammar: a dim
// right-aligned label, a fixed gutter, then the value. Ours printed
// "session cli-...", "provider config" and "model gpt-5.5" with no shared
// column, so nothing lined up and the block read as three unrelated lines. The
// gutter is what makes it a table instead of a list.
type block struct {
	rows [][2]string
	// title, when set, is a bold heading above the rows.
	title string
	// note, when set, is a dim line under the block.
	note string
}

// add appends a row. Empty labels are kept rather than skipped, so a deliberately
// blank value still shows its label and the block keeps a stable shape.
func (b *block) add(label, value string) { b.rows = append(b.rows, [2]string{label, value}) }

// addIf appends only when the value is non-empty, which is how an unconfigured
// provider disappears instead of printing "provider" with nothing after it.
func (b *block) addIf(label, value string) {
	if strings.TrimSpace(value) != "" {
		b.add(label, value)
	}
}

// gutter is the widest label plus a gap, bounded so one long label cannot push
// every value to the right edge of the screen.
func (b *block) gutter() int {
	widest := 0
	for _, r := range b.rows {
		if n := runeLen(r[0]); n > widest {
			widest = n
		}
	}
	if widest > 12 {
		widest = 12
	}
	return widest + 2
}

func runeLen(s string) int { return len([]rune(s)) }

// unset is what an empty value renders as.
//
// An empty cell reads as a rendering fault rather than as an absent setting: the
// label is there, so the reader looks for a value and finds trailing whitespace.
// A fresh install with no provider configured printed "model" followed by nothing
// and looked broken when nothing was wrong with it.
const unset = "unset"

// escapeEnd returns the index just past the escape sequence starting at i.
//
// Only CSI sequences (`ESC [ ... final`) are recognised, which is every SGR a
// Palette emits. Any other escape is taken as ESC plus one byte, so an unknown
// sequence is consumed rather than counted as visible text.
func escapeEnd(rs []rune, i int) int {
	if i+1 >= len(rs) {
		return i + 1
	}
	if rs[i+1] != '[' {
		return i + 2
	}
	j := i + 2
	for j < len(rs) && !ansiFinal(rs[j]) {
		j++
	}
	if j < len(rs) {
		j++
	}
	return j
}

// ansiFinal reports whether r terminates a CSI sequence: the final byte of a CSI
// is in the range 0x40-0x7E, and the parameter bytes before it are 0x30-0x3F.
func ansiFinal(r rune) bool { return r >= 0x40 && r <= 0x7e }

// visibleWidth is the number of terminal cells a styled string occupies.
//
// Counting runes is wrong the moment the palette is on: an SGR sequence is
// several runes and zero cells, so a line measured with those runes in the count
// is judged wider than it looks and is truncated several characters early. That
// is what cut `no provider endpoint configured f…`, `/help for commands … int…`
// and `error · session cli-1790795697…` mid-word on a narrow screen: the line
// had four real cells left and four of them were spent on escape bytes.
func visibleWidth(s string) int {
	n := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '\x1b' {
			n++
			continue
		}
		i = escapeEnd(rs, i) - 1
	}
	return n
}

// cutCells returns the prefix of s that occupies at most limit cells.
//
// Escape sequences are carried through without costing a cell, so a cut can
// never land inside one and leave an unbalanced sequence behind; the caller
// closes a run that it had to break.
func cutCells(s string, limit int) string {
	var b strings.Builder
	n := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if rs[i] == '\x1b' {
			j := escapeEnd(rs, i)
			b.WriteString(string(rs[i:j]))
			i = j - 1
			continue
		}
		if n == limit {
			return b.String()
		}
		b.WriteRune(rs[i])
		n++
	}
	return b.String()
}

// render draws the block. The label is dim and the value is not, so the eye lands
// on the value without needing a colour.
func (b *block) render(p Palette, indent string, width int) []string {
	var out []string
	if b.title != "" {
		// Trimmed, because the indent is applied here and a title that carried its
		// own landed a column right of the rows beneath it. That is invisible to a
		// test that only looks for the title's text, which is why it survived.
		out = append(out, p.Bold(indent+strings.TrimSpace(b.title)))
	}
	gut := b.gutter()
	for _, r := range b.rows {
		label := r[0]
		// Left-aligned into the gutter, so the labels share an edge and the
		// values share a column. They used to be right-aligned, which reads worse
		// the moment there is more than one row: the left edge came out ragged,
		// and a single-row block came out indented by one for no reason at all,
		// which is the whole startup block shifted off its own margin.
		pad := gut - runeLen(label)
		if pad < 1 {
			pad = 1
		}
		value := r[1]
		if strings.TrimSpace(value) == "" {
			// Dim, so an absent setting recedes the way a missing label does.
			value = p.Dim(unset)
		}
		// The value is appended only when it is non-empty, which is also what
		// keeps a blank cell from shipping trailing whitespace to the terminal.
		line := indent + p.Dim(label) + strings.Repeat(" ", pad) + value
		out = append(out, strings.TrimRight(p.fit(line, width), " "))
	}
	if b.note != "" {
		out = append(out, p.Dim(indent+b.note))
	}
	return out
}

// fit truncates a line to the terminal width by display width.
//
// Three things a naive cut gets wrong, and all three were on screen:
//
//   - It measured runes, so a styled line lost a cell for every escape byte and
//     came up short of the width it was allowed.
//   - It cut at a fixed cell, so a word was left as `f…` and `int…`, where the
//     reader has to guess the rest instead of reading a whole line.
//   - It could cut before a reset, leaving the terminal styled after the line.
//
// So: cut by visible cells, pull back to the last space when that keeps most of
// the line, and close the run if it was still open.
func (p Palette) fit(s string, width int) string {
	if width < 4 || visibleWidth(s) <= width {
		return s
	}
	kept := cutCells(s, width-1)
	// Pull back to a word boundary only when that does not throw most of the line
	// away: a line of one long token, such as a URL or an id, has no space to
	// break at and a hard cut is the only option left.
	if i := strings.LastIndexByte(kept, ' '); i > 0 && visibleWidth(kept[:i])*2 >= (width-1) {
		kept = kept[:i]
	}
	if p.color && sgrOpen(kept) {
		kept += ansiReset
	}
	return kept + "…"
}

// sgrOpen reports whether the last style sequence in s left an attribute on.
//
// The palette only ever opens with bold, dim, reverse or underline and closes
// with a reset, so "open" is simply "the final SGR is not a reset".
func sgrOpen(s string) bool {
	rs := []rune(s)
	open := false
	for i := 0; i < len(rs); i++ {
		if rs[i] != '\x1b' {
			continue
		}
		j := escapeEnd(rs, i)
		seq := string(rs[i:j])
		// Only an SGR (`ESC [ ... m`) carries attributes.
		if strings.HasPrefix(seq, "\x1b[") && strings.HasSuffix(seq, "m") {
			params := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b["), "m")
			open = params != "" && params != "0"
		}
		i = j - 1
	}
	return open
}

// elide shortens a list to fit, naming how many were dropped.
//
// Aster prints "chat_history ... +16 more" rather than wrapping a long list, and
// the count is the useful part: a reader learns the surface is larger than the
// line without needing the whole list.
func elide(items []string, max int) (shown string, more int) {
	if len(items) <= max {
		return strings.Join(items, ", "), 0
	}
	shown = strings.Join(items[:max], ", ")
	return shown, len(items) - max
}

// kv is a one-line key/value for the footer, where a block would be too tall.
func kv(label, value string) string {
	if value == "" {
		return ""
	}
	return label + " " + value
}

// dotRule is the separator between footer segments.
//
// A middle dot rather than a pipe or a comma: a pipe reads as a table edge and a
// comma reads as prose, while a centred dot is decoration that carries no meaning
// and therefore cannot be misread as one.
const dotRule = "·"

// joinDots joins non-empty segments with the separator and two spaces.
func joinDots(segments ...string) string {
	kept := make([]string, 0, len(segments))
	for _, s := range segments {
		if s = strings.TrimSpace(s); s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, "  "+dotRule+"  ")
}

// statusLine is the persistent context line: what the next turn will run with.
//
// Aster keeps a footer showing mode, model and effort permanently. Ours printed
// nothing between turns, so the model in use was only visible in the banner that
// scrolled away. This is a single dim line, redrawn at every prompt, and it is
// the one line that answers "what am I about to talk to".
//
// On a narrow screen it degrades by dropping whole fields rather than by being
// cut through the middle: the session id is traceability and the provider prefix
// repeats what the model already says, so those go first, and the mode - what the
// run is actually doing - is what survives. Truncating the assembled line instead
// left `session cli-1790795697` with the rest of the id gone, which is worse than
// showing no id at all, because it looks like an id.
func (p Palette) statusLine(width int, model, provider, mode, session string) string {
	withSession := joinDots(kv(provider, model), mode, kv("session", session))
	noSession := joinDots(kv(provider, model), mode)
	noProvider := joinDots(model, mode)
	modeOnly := mode
	for _, line := range []string{withSession, noSession, noProvider, modeOnly} {
		if line == "" {
			continue
		}
		if visibleWidth("  "+line) <= width {
			return p.Dim("  " + line)
		}
	}
	if mode == "" {
		return ""
	}
	// Even the mode is wider than the screen; cut it, but at a word boundary.
	return p.fit(p.Dim("  "+mode), width)
}

// rule is a short divider, not a full-width one.
//
// The footer used a 70-character rule of dashes under three short numbers, which
// is more ink than the content. A short rule marks the boundary without drawing
// a line across the screen.
func (p Palette) rule(width int) string {
	n := 24
	if width/2 < n {
		n = width / 2
	}
	if n < 6 {
		n = 6
	}
	return p.Dim(strings.Repeat("─", n))
}

// hintBar is the single line of key hints shown under the composer.
//
// Aster keeps this to one line and names the most useful keys rather than all of
// them; ours printed a full sentence on every launch. It is shown once, at the
// first prompt, and never repeated.
//
// Hints are dropped from the end as width runs out. `/help` is the one hint that
// always earns its place - it is the door to the rest - while the interrupt and
// quit keys are discoverable from /help, so a narrow terminal shows less rather
// than showing `ctrl-c int…`, which names no key at all.
func (p Palette) hintBar(width int) string {
	all := joinDots("/help for commands", "ctrl-c interrupts", "ctrl-d quits")
	basics := joinDots("/help for commands", "ctrl-c interrupts")
	one := "/help for commands"
	for _, line := range []string{all, basics, one} {
		if visibleWidth("  "+line) <= width {
			return p.Dim("  " + line)
		}
	}
	return p.fit(p.Dim("  "+one), width)
}

// glyph is the marker in front of a prompt or a result line.
//
// Aster uses a coloured chevron for the composer and small geometric marks for
// results. In monochrome the same vocabulary is carried by shape alone, so the
// marks are chosen to be distinct as glyphs and not only as colours.
func glyph(kind string) string {
	switch kind {
	case "prompt":
		return "❯"
	case "ok":
		return "•"
	case "warn":
		return "!"
	case "error":
		return "×"
	case "info":
		return "·"
	default:
		return "·"
	}
}

// formatToolLine renders a tool start or finish as one aligned line.
func formatToolLine(status, name, detail string) string {
	parts := []string{}
	if detail != "" {
		parts = append(parts, detail)
	}
	line := glyph(status) + " " + name
	if len(parts) > 0 {
		line += "  " + strings.Join(parts, "  ")
	}
	return line
}

// summariseRun is the footer of a turn: tokens, cost, cache, and the duration.
//
// Aster's footer is one dim line of dot-separated segments with no rule above it.
// Ours used a full-width rule and a bare list, which read as a wall between the
// answer and the prompt.
func summariseRun(width int, p Palette, in, out, cache, reasoning, cost, elapsed string) string {
	all := []string{kv("in", in), kv("out", out), kv("cache", cache),
		kv("think", reasoning), cost, elapsed}
	// Drop from the tail so the two numbers that answer "how big was this turn"
	// survive a narrow screen, and the decorative tail goes first.
	for n := len(all); n > 0; n-- {
		seg := joinDots(all[:n]...)
		if seg == "" {
			continue
		}
		if visibleWidth("  "+seg) <= width {
			return p.Dim("  " + seg)
		}
	}
	return ""
}

// plural returns "" or "s" for a count, so a line does not say "1 tools".
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// describeCount renders "3 tools" or "1 tool".
func describeCount(n int, noun string) string {
	return fmt.Sprintf("%d %s%s", n, noun, plural(n))
}
