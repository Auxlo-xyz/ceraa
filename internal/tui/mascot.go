package tui

import "strings"

// The mark is Ceraa's face at logo size, drawn from mascot.PNG: a rounded
// three-lobed cloud, one large open oval eye and one wide flat eye that reads
// as a wink. It is written in block characters so it needs no image support, no
// colour and no font beyond a terminal's default.
//
// It was rasterised, not hand-drawn: the white cloud on black, cropped to the
// bytes that hold ink (x 233-1016, y 228-1037 of the 1254 square), scaled to 20
// wide, thresholded at half brightness, and paired into half-block rows so 11
// lines carry 22 scanlines. The open eye survives as the wide void left of
// centre and the wink as the short notch to its right; at this size neither is
// geometry you could measure off the PNG, but both are gaps, and the test below
// holds the gaps rather than the rows.
//
// The two-space indent is part of the constant. It matches the detail block the
// mark stands over, and keeping it here lets fitArt spend it when the terminal
// narrows instead of special-casing the logo.
const mascotMark = `        ▄▄████▄
       █████████▄
    ▄▄██████████████▄
  ▄█████▀▀███████████▄
  █████    ███████████
  █████    ██  ▄  ███
   ▀███    ███████████
   ███████████████████
   ▀█████████████████▀
    ▀▀███████████▀▀▀
         ▀█████▀`

// State is what the mascot is currently doing.
//
// A run is a state machine the renderer advances, and the mascot is a pure
// function of it, so nothing about the drawing can drift from what the run is
// actually doing.
type State int

const (
	// StateIdle is between turns.
	StateIdle State = iota
	// StateThinking is a request in flight with no tool running yet.
	StateThinking
	// StateWorking is at least one tool executing.
	StateWorking
	// StateDone is a completed turn.
	StateDone
	// StateError is a failed turn.
	StateError
)

// String names a state, for the status line and for tests.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateThinking:
		return "thinking"
	case StateWorking:
		return "working"
	case StateDone:
		return "done"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// spinnerFrames is a braille cycle, which advances in one cell so the status line
// does not reflow while it turns.
//
// Braille is used rather than the more common `|/-\` because it reads as motion
// at a glance and does not suggest a progress bar that is not there.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner returns the frame for a tick, wrapping on tick count.
//
// The modulo is on the frame count rather than being clamped, so a long run keeps
// turning instead of freezing on the last frame, and a negative tick from a clock
// adjustment cannot index out of range.
func Spinner(tick int) string {
	n := len(spinnerFrames)
	return spinnerFrames[((tick%n)+n)%n]
}

// Banner is the startup block: the mark, the name and version, and the
// resolved provider, so the first thing on screen says which model is answering
// before a prompt is typed.
//
// The details go through the same aligned block as /status rather than a
// hand-counted pad, so the two cannot disagree about where the value column is.
//
// The mark is shown only when it fits. A row the terminal has to wrap turns the
// drawing into a smear of orphaned cells, which is the one failure nothing in
// the block can survive because none of it is text. fitArt gives up the indent
// before it gives up any of the drawing, and reports false when even that is
// not enough; a logo missing a slice of its edge is worse than no logo.
func (p Palette) Banner(version, model, provider string, width int) string {
	var b []byte
	if art, ok := fitArt(mascotMark, width); ok {
		b = append(b, p.Bold(art)...)
		b = append(b, '\n')
	}
	b = append(b, p.Bold("  Ceraa")+p.Dim("  "+version+"\n")...)
	blk := &block{}
	blk.addIf("model", model)
	blk.addIf("provider", provider)
	if len(blk.rows) > 0 {
		b = append(b, []byte(strings.Join(blk.render(p, "  ", width), "\n")+"\n")...)
	}
	return string(b)
}

// fitArt narrows a block of art to width by spending its indent, never its art.
//
// Every row is shifted left by the same amount, so the shape is untouched: only
// the padding that centres it is spent. It answers false when the full indent is
// still not enough, and the caller then drops the art entirely.
func fitArt(art string, width int) (string, bool) {
	if width <= 0 {
		return "", false
	}
	lines := strings.Split(art, "\n")
	maxW, indent := 0, -1
	for _, l := range lines {
		// A row of pure padding is not part of the shape, and must not be the
		// thing every other row is measured against.
		if strings.TrimSpace(l) == "" {
			continue
		}
		// Counted in runes, not bytes: the block characters are three bytes each
		// and one cell, so len() here would call a 42-cell face 120 wide and drop
		// it on every terminal that fits it.
		if w := runeLen(l); w > maxW {
			maxW = w
		}
		n := 0
		for _, r := range l {
			if r != ' ' {
				break
			}
			n++
		}
		if indent < 0 || n < indent {
			indent = n
		}
	}
	if maxW <= width {
		return art, true
	}
	strip := maxW - width
	if indent < 0 || strip > indent {
		return "", false
	}
	for i, l := range lines {
		if len(l) >= strip {
			lines[i] = l[strip:]
		}
	}
	return strings.Join(lines, "\n"), true
}
