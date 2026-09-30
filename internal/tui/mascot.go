package tui

import "strings"

// The mascot is Ceraa's face, drawn from mascot.png: a rounded three-lobed
// cloud, one large open oval eye and one wide flat eye that reads as a wink.
// It is written in block characters so it needs no image support, no colour and
// no font beyond a terminal's default.
//
// The PNG is the source of truth for the geometry, and the numbers came from
// measuring it rather than from guessing. The cloud body occupies 784x810 pixels
// of the source. The two features were found by flood-filling the black regions
// enclosed by white, which gives the open eye at x 190-356 y 268-535, a tall
// oval 167 wide by 268 high, and the wink at x 412-644 y 350-448, a wide flat
// shape 233 wide by 99 high. Both sit on roughly the same line (y 400), which is
// what makes the face read as a wink rather than as two eyes looking around.
//
// An earlier version of this drew a generic rounded face with two dot eyes and
// a straight mouth. That is a different character: the wink and the lobed cloud
// are the entire personality, and a drawing that keeps neither is not this
// mascot however legible it is.
//
// The five states keep one silhouette and swap only the features, so the face is
// recognisable at a glance and the banner does not jump shape as a run changes
// state.
const (
	// mascot is the whole drawing: the three-lobed cloud from mascot.PNG with
	// its tall open eye and its flat wink.
	//
	//	// The cloud is drawn in the FOREGROUND, not filled with block characters.
	// The PNG is a white cloud on black, and a terminal is normally light text
	// on a dark background. Filling the shape made the face render as two holes
	// punched in a black slab: the silhouette vanished and only the eyes
	// survived. Drawn as ink it is the cloud, and the eyes are the absence of
	// ink, which is what the PNG actually is.
	//
	// Half-blocks double the vertical resolution, so the 16 rows below carry the
	// 32 scanlines the shape needs to stay round rather than squashed.
	mascot = `               ▄▄▄████████▄▄
             ▄████████████████▄
           ▄███████████████████▄▄▄▄▄
      ▄▄██████████████████████████████▄▄
   ▄█████████████████████████████████████
  ▄██████████▀    ▀███████████████████████
  ██████████        ██████▀▀▀▀▀▀█████████▀
  ▀█████████        ███▀   ▄▄▄▄   ▀█████▀
   ▀████████        ████▄████████▄███████
     ▄███████▄▄  ▄▄███████████████████████
     █████████████████████████████████████
     ████████████████████████████████████
      ▀███████████████████████████████▀▀
         ▀▀▀▀▀▀▀███████████████▀
                 ▀▀████████▀▀▀`
)

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

// Banner is the startup block: the mascot, the name and version, and the
// resolved provider, so the first thing on screen says which model is answering
// before a prompt is typed.
//
// The details go through the same aligned block as /status rather than a
// hand-counted pad, so the two cannot disagree about where the value column is.
//
// The art is shown only when it fits. The face is 42 cells across, and a
// terminal narrower than that hard-wraps each row, which turns the block into a
// smear of orphaned cells - the same failure as every other line here, and the
// one that is hardest to read because nothing in it is text. fitArt gives up the
// indent that centres the face before it gives up any of the face, and reports
// false when even that is not enough; a mascot missing a slice of its cheek is
// worse than no mascot.
func (p Palette) Banner(version, model, provider string, width int) string {
	var b []byte
	if art, ok := fitArt(mascot, width); ok {
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
