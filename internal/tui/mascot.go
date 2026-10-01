package tui

import "strings"

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

// Banner is the startup block: the name and version, and the resolved provider,
// so the first thing on screen says which model is answering before a prompt is
// typed.
//
// There is no drawing. The session opened with a face at three sizes and every
// one of them was wrong for a terminal: full size filled the phone screen,
// small still spent eleven rows on brand before saying anything. Nothing here
// earns those rows, so the face is gone from this surface entirely rather than
// tuned again. The PNG stays in the repository for a surface with pixels.
//
// The details go through the same aligned block as /status rather than a
// hand-counted pad, so the two cannot disagree about where the value column is.
func (p Palette) Banner(version, model, provider string, width int) string {
	var b []byte
	b = append(b, p.Bold("  Ceraa")+p.Dim("  "+version+"\n")...)
	blk := &block{}
	blk.addIf("model", model)
	blk.addIf("provider", provider)
	if len(blk.rows) > 0 {
		b = append(b, []byte(strings.Join(blk.render(p, "  ", width), "\n")+"\n")...)
	}
	return string(b)
}
