package tui

// The mascot is Ceraa's face: a soft cloud shape with one open eye and one that
// is closed in a wink, drawn from mascot.svg in block characters so it needs no
// image support, no colour, and no font beyond a terminal's default.
//
// The states are the same face with the features swapped rather than separate
// drawings. One silhouette that a user recognises in a half second is worth more
// than five distinct pictures, and it keeps the banner from jumping between
// shapes as a run changes state.
const (
	// mascotIdle is the resting wink.
	mascotIdle = `        .-~~~-.  
     .'  o   o  '.  
    /   ___       \ 
   |   '---'       | 
    \             / 
     '-._______.-' `

	// mascotThinking narrows the open eye and tightens the mouth, which reads as
	// concentration without needing motion.
	mascotThinking = `        .-~~~-.  
     .'  -   -  '.  
    /   _-_       \ 
   |   '---'       | 
    \             / 
     '-._______.-' `

	// mascotWorking opens the wink into a mouth, so a run that is doing something
	// looks different from one that is only waiting.
	mascotWorking = `        .-~~~-.  
     .'  o   o  '.  
    /   ___       \ 
   |   \_ _/       | 
    \             / 
     '-._______.-' `

	// mascotDone is the wink with a raised brow, the same face at rest after work.
	mascotDone = `        .-~~~-.  
     .'  o   ^  '.  
    /   ___       \ 
   |   '---'       | 
    \             / 
     '-._______.-' `

	// mascotError keeps the silhouette and changes only the mouth, so a failure is
	// still unmistakably Ceraa.
	mascotError = `        .-~~~-.  
     .'  o   o  '.  
    /   ___       \ 
   |   \_/         | 
    \             / 
     '-._______.-' `
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

// face returns the drawing for a state.
func (s State) face() string {
	switch s {
	case StateThinking:
		return mascotThinking
	case StateWorking:
		return mascotWorking
	case StateDone:
		return mascotDone
	case StateError:
		return mascotError
	default:
		return mascotIdle
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

// RenderMascot draws the mascot for a state, in bold when colour is on.
//
// The whole block is emitted as one write so the face cannot be torn by an
// interleaved write from a concurrent tool callback.
func (p Palette) RenderMascot(s State) string { return p.Bold(s.face()) }

// Banner is the startup block: the mascot, the name, and the resolved provider,
// so the first thing on screen says which model is answering before any prompt
// is typed.
func (p Palette) Banner(version, model, provider string) string {
	var b []byte
	b = append(b, p.Bold(mascotIdle)...)
	b = append(b, '\n')
	b = append(b, p.Bold("  Ceraa")...)
	b = append(b, p.Dim("  "+version+"\n")...)
	if model != "" {
		b = append(b, p.Dim("  model    ")+p.Bold(model)+"\n"...)
	}
	if provider != "" {
		b = append(b, p.Dim("  provider ")+provider+"\n"...)
	}
	return string(b)
}
