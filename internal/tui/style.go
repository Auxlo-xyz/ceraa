// Package tui renders Ceraa runs in a terminal: a black and white theme, the
// mascot, and the interactive loop that drives the coordinator.
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

// Bold renders emphasised text: the mascot, headings, and the final answer's
// leading marker.
func (p Palette) Bold(s string) string { return p.wrap(ansiBold, s) }

// Dim renders de-emphasised text: timestamps, counts, and anything that is
// context rather than content.
func (p Palette) Dim(s string) string { return p.wrap(ansiDim, s) }

// Reverse renders the selected status pill, which is how the current state is
// marked without spending a colour.
func (p Palette) Reverse(s string) string { return p.wrap(ansiReverse, s) }

// Underline renders a heading rule.
func (p Palette) Underline(s string) string { return p.wrap(ansiUnderline, s) }

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

// wrapText hard-wraps to n columns on word boundaries.
//
// Model output is unwrapped prose and a 200 column terminal turns every
// paragraph into a unreadable ribbon. A word longer than the limit, such as a
// path or a base64 blob, is broken rather than allowed to overflow, because an
// overflowing line wraps on its own and corrupts the layout of everything after
// it.
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
		line := ""
		for _, word := range strings.Fields(para) {
			for len(word) > n {
				if line != "" {
					out = append(out, line)
					line = ""
				}
				out = append(out, word[:n])
				word = word[n:]
			}
			switch {
			case line == "":
				line = word
			case len(line)+1+len(word) <= n:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
