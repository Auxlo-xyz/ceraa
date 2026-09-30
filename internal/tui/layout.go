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

// render draws the block. The label is dim and the value is not, so the eye lands
// on the value without needing a colour.
func (b *block) render(p Palette, indent string, width int) []string {
	var out []string
	if b.title != "" {
		out = append(out, p.Bold(indent+b.title))
	}
	gut := b.gutter()
	for _, r := range b.rows {
		label := r[0]
		// Right-align the label into the gutter, so the values form a column.
		pad := gut - runeLen(label) - 1
		if pad < 0 {
			pad = 0
		}
		line := indent + strings.Repeat(" ", pad) + p.Dim(label) + "  " + r[1]
		out = append(out, p.fit(line, width))
	}
	if b.note != "" {
		out = append(out, p.Dim(indent+b.note))
	}
	return out
}

// fit truncates a line to the terminal width by display width.
//
// Truncation is by rune with a trailing ellipsis rather than by byte, so a
// multi-byte glyph at the boundary cannot split into a replacement character.
func (p Palette) fit(s string, width int) string {
	if width < 12 || runeLen(s) <= width {
		return s
	}
	r := []rune(s)
	// Reserve one cell for the ellipsis.
	keep := width - 1
	if keep < 1 {
		keep = 1
	}
	return string(r[:keep]) + "…"
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
func (p Palette) statusLine(width int, model, provider, mode, session string) string {
	line := joinDots(
		kv(provider, model),
		mode,
		kv("session", session),
	)
	return p.fit(p.Dim("  "+line), width)
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
func (p Palette) hintBar(width int) string {
	return p.fit(p.Dim("  /help for commands  "+dotRule+"  ctrl-c interrupts  "+dotRule+"  ctrl-d quits"), width)
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
	seg := joinDots(
		kv("in", in), kv("out", out), kv("cache", cache),
		kv("think", reasoning), cost, elapsed,
	)
	if seg == "" {
		return ""
	}
	return p.fit(p.Dim("  "+seg), width)
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
