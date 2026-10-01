// Package tui renders Ceraa runs in a terminal: a black and white theme, the
// status line, and the interactive loop that drives the coordinator.
//
// The palette is deliberately monochrome. A coding agent spends hours looking at
// this, and colour here would only encode state the glyphs already carry, so a
// dimmed grey and a bold white is the whole system. Every escape sequence is
// emitted through a Palette whose colour output can be turned off wholesale, so
// a piped run stays clean text.
package tui

import (
	"os"
	"strings"

	"github.com/mattn/go-isatty"
)

// term is reached through a variable so a test can claim a terminal without
// one, since the palette and the width both branch on it.
var term = struct{ isTerminal func(uintptr) bool }{
	isatty.IsTerminal,
}

// ANSI select graphic rendition. Reset is 0 and is always written last so a
// truncated write cannot leave the terminal in a styled state.
const (
	ansiReset     = "\x1b[0m"
	ansiBold      = "\x1b[1m"
	ansiDim       = "\x1b[2m"
	ansiReverse   = "\x1b[7m"
	ansiUnderline = "\x1b[4m"
	// ansiBrightWhite is SGR 97, the one step above the default foreground. It is
	// the accent the product name uses, chosen because it is the only step every
	// terminal agrees on: no 256 colour, no true colour, no theme.
	ansiBrightWhite = "\x1b[97m"
)

// Palette emits the theme, or plain text when colour is off.
//
// A Palette is a value and carries no state, so every helper takes one and two
// palettes can never disagree about whether colour is enabled.
type Palette struct{ color bool }

// NewPalette reports whether colour should be used.
//
// The terminal check is the authority rather than a flag alone: piping to a
// file or a pager must produce plain text even when the caller believes it is on
// a terminal, and escape codes in a log file are noise. NO_COLOR is honoured
// because that is the cross-tool convention and a user who set it is asking.
func NewPalette(force bool) Palette {
	if os.Getenv("NO_COLOR") != "" {
		return Palette{}
	}
	if !force {
		return Palette{color: term.isTerminal(os.Stdout.Fd())}
	}
	return Palette{color: true}
}

// Plain is a palette with colour disabled, for tests and for piped output.
func Plain() Palette { return Palette{} }

// Color reports whether this palette emits escape sequences.
func (p Palette) Color() bool { return p.color }

func (p Palette) wrap(code, s string) string {
	if !p.color || s == "" {
		return s
	}
	return code + s + ansiReset
}

// Bold renders emphasised text: headings, and the final answer's
// leading marker.
func (p Palette) Bold(s string) string { return p.wrap(ansiBold, s) }

// Dim renders de-emphasised text: timestamps, counts, and anything that is
// context rather than content.
func (p Palette) Dim(s string) string { return p.wrap(ansiDim, s) }

// Reverse renders the selected status pill, which is how the current state is
// marked without spending a colour.
func (p Palette) Reverse(s string) string { return p.wrap(ansiReverse, s) }

// Code renders a code span in reverse video.
//
// A terminal has one dependable way to say "this is a literal", and it is the one
// every pager, diff tool and tty already uses. It is also the only treatment that
// needs no colour scheme, which is the constraint this whole palette is built
// under.
func (p Palette) Code(s string) string { return p.wrap(ansiReverse, s) }

// Underline renders a heading rule.
func (p Palette) Underline(s string) string { return p.wrap(ansiUnderline, s) }

// Name renders the product's own name, and it is the one thing on screen that is
// allowed to be the brightest thing there.
//
// Everything else here is dim or bold, and bold alone reads as emphasis rather
// than as identity, so the name used to look like a heading among other
// headings. Bright white is one level above the default foreground, which every
// terminal honours, so the emphasis survives on a phone emulator and on a
// terminal with an eight colour palette without asking either of them for a
// colour scheme. Under NO_COLOR this is the default foreground, which is the
// correct fallback rather than a degraded one.
func (p Palette) Name(s string) string { return p.wrap(ansiBrightWhite, s) }

// Invert is the bare escape, for the spinner frames that repaint in place.
func (p Palette) Invert() string {
	if !p.color {
		return ""
	}
	return ansiReverse
}

// NoInvert closes a bare reverse that was opened without a payload to reset.
func (p Palette) NoInvert() string {
	if !p.color {
		return ""
	}
	return ansiReset
}

// BoldOn and BoldOff bracket a run of streamed text so the answer is emphasised
// while it is being written, without needing to know where the answer ends.
func (p Palette) BoldOn() string {
	if !p.color {
		return ""
	}
	return ansiBold
}

// BoldOff closes BoldOn.
func (p Palette) BoldOff() string {
	if !p.color {
		return ""
	}
	return ansiReset
}

// DimOn opens a dim run for streamed reasoning, so the stream does not need to
// know where reasoning ends.
func (p Palette) DimOn() string {
	if !p.color {
		return ""
	}
	return ansiDim
}

// Reset closes any open run.
func (p Palette) Reset() string {
	if !p.color {
		return ""
	}
	return ansiReset
}

// width reports the terminal width, for callers that need it to lay out a line.
func (p Palette) width() int { return width() }

// Width reports the terminal width.
func (p Palette) Width() int { return width() }

// width reports the usable column count.
//
// The terminal is asked through its ioctl, and a failure or a nonsense answer
// falls back to 80 rather than propagating: a wrong wrap is cosmetic, and a
// terminal that cannot be measured should not stop a run.
func width() int {
	if !term.isTerminal(os.Stdout.Fd()) {
		return 80
	}
	w, _, err := terminalSize(os.Stdout.Fd())
	if err != nil || w < 20 {
		return 80
	}
	return w
}

// WrapText hard-wraps s to n display cells on word boundaries, preserving blank
// lines and existing line breaks.
//
// Exported so the command help lays out the same way model output does. Cobra's
// default template leaves long descriptions alone, and a terminal left alone
// cuts them wherever the edge is, which on a narrow screen is the middle of a
// word. A help page laid out here never gives the terminal that chance.
func WrapText(s string, n int) []string { return wrapText(s, n) }

// wrapText hard-wraps to n display cells on word boundaries.
//
// Model output is unwrapped prose and a 200 column terminal turns every
// paragraph into a unreadable ribbon. A word longer than the limit, such as a
// path or a base64 blob, is broken rather than allowed to overflow, because an
// overflowing line wraps on its own and corrupts the layout of everything after
// it.
//
// Two rules the first version got wrong and the screen showed:
//
//   - A line that already fits is emitted untouched. Our own rows are
//     pre-formatted - an indent, a label, a padded description - and splitting
//     those on spaces destroys the alignment they exist for. `/help` printed as
//     `hel` / `o` precisely because the padding was fed through a splitter that
//     was built for prose.
//   - Width is measured in cells, not bytes, and an escape sequence is carried
//     with the text it styles instead of becoming a word of its own.
func wrapText(s string, n int) []string {
	if n < 8 {
		n = 8
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		if strings.TrimSpace(para) == "" {
			out = append(out, "")
			continue
		}
		if visibleWidth(para) <= n {
			out = append(out, para)
			continue
		}
		out = append(out, wrapParagraph(para, n)...)
	}
	return out
}

// wrapParagraph word-wraps one paragraph that is wider than n cells.
//
// The leading indent stays on the first line and continuation lines start at the
// margin: hanging every wrapped line under the original indent reads as a new
// paragraph rather than as a continuation.
func wrapParagraph(para string, n int) []string {
	rs := []rune(para)
	k := 0
	for k < len(rs) && (rs[k] == ' ' || rs[k] == '\t') {
		k++
	}
	indent := rs[:k]

	var (
		out    []string
		line   []rune
		cells  int
		lastSp int // index in line of the last space a line may break at
	)
	flush := func() {
		if len(line) == 0 {
			return
		}
		out = append(out, strings.TrimRight(string(line), " "))
		line = nil
		cells = 0
		lastSp = -1
	}
	line = append(line, indent...)
	cells = visibleWidth(string(indent))
	lastSp = -1

	i := k
	for i < len(rs) {
		switch {
		case rs[i] == '\x1b':
			// A style sequence costs no cell and belongs to whatever it wraps, so
			// it travels with the line it is on. SGR state survives a newline, so
			// an attribute opened before the break still applies after it and its
			// reset, which is still to come, still lands.
			j := escapeEnd(rs, i)
			line = append(line, rs[i:j]...)
			i = j

		case rs[i] == ' ':
			if cells+1 > n {
				flush()
				for i < len(rs) && rs[i] == ' ' {
					i++
				}
				continue
			}
			line = append(line, ' ')
			cells++
			lastSp = len(line) - 1
			i++

		default:
			j := i
			for j < len(rs) && rs[j] != ' ' && rs[j] != '\x1b' {
				j++
			}
			word := rs[i:j]
			if cells+len(word) <= n {
				line = append(line, word...)
				cells += len(word)
				i = j
				continue
			}
			if lastSp > 0 {
				// Move the word and everything after the last space down as one,
				// then re-test the word against the new, shorter line rather than
				// assuming it fits: a word longer than the limit would otherwise
				// be appended past the boundary it was just moved to avoid.
				tail := append([]rune{}, line[lastSp+1:]...)
				out = append(out, strings.TrimRight(string(line[:lastSp]), " "))
				line = tail
				cells = visibleWidth(string(line))
				lastSp = -1
				continue
			}
			// No space to break at: hard-fill, splitting the word at the edge.
			for len(word) > 0 {
				if cells >= n {
					flush()
				}
				room := n - cells
				take := len(word)
				if take > room {
					take = room
				}
				if take <= 0 {
					flush()
					continue
				}
				line = append(line, word[:take]...)
				cells += take
				word = word[take:]
			}
			i = j
		}
	}
	if len(line) > 0 {
		out = append(out, strings.TrimRight(string(line), " "))
	}
	return out
}
