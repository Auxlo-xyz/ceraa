package tui

import (
	"strconv"
	"strings"
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

// Startup is what the session knows about itself before anyone types.
//
// A struct rather than a parameter list, because the list grew to eight fields
// and a caller passing them positionally had to count. Order is irrelevant here
// and only the label order in the rendered block matters.
type Startup struct {
	Version  string
	Model    string
	Provider string
	WorkDir  string
	Effort   string
	Agent    string
	Tools    int
	Session  string
}

// Banner is the startup block: the name, and every piece of state that decides
// what the first turn will do.
//
// It answers the question the operator actually has at that moment, which is
// "am I about to talk to the thing I think I am". Model, provider, cwd, effort
// and tool count are all on the wire for the next request, and a mismatch in any
// of them is the difference between a working session and a confusing one. It
// used to print the provider and nothing else, which left five of those
// questions open and made the block look empty next to a competitor's.
//
// The rows go through the same aligned block as /status rather than a
// hand-counted pad, so the two cannot disagree about where the value column is.
func (p Palette) Banner(start Startup, width int) string {
	var b []byte
	b = append(b, "  "+p.Name("Ceraa")+p.Dim("  "+start.Version+"\n")...)
	blk := &block{}
	// Added rather than skipped when empty, so an unconfigured install shows
	// unset. Omitting the row would leave the operator wondering whether the
	// session forgot to report a model or simply has none, which are different
	// problems with different fixes.
	blk.add("model", start.Model)
	blk.add("provider", start.Provider)
	blk.add("cwd", start.WorkDir)
	blk.add("effort", start.Effort)
	blk.addIf("agent", start.Agent)
	if start.Tools > 0 {
		blk.add("tools", strconv.Itoa(start.Tools))
	}
	blk.add("session", start.Session)
	if len(blk.rows) > 0 {
		b = append(b, []byte(strings.Join(blk.render(p, "  ", width), "\n")+"\n")...)
	}
	return string(b)
}
