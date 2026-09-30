package tui

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
	// mascot is the whole drawing: a three-lobed cloud, one tall open oval
	// eye and one wide flat eye beside it that reads as a wink.
	//
	// It is static. An earlier version carried five states and swapped
	// the features between them, which was animation nobody asked for and
	// five drawings to keep in sync with the PNG they came from.
	mascot = `             ▄▄████████▄▄
          ▄███████████████▄
         ███████████████████▄▄▄▄▄
    ▄▄█████████████████████████████▄
 ▄███████████████████████████████████
▄█████████▀▀████████████▀▀▀▀▀█████████▄
██████████    ████▀▀▀▀▀▀▀▀▀▀██████████
▀████████     ██▀  ▀▀▀▀▀▀▀▀▀▀  ▀▀█████▀
 ▀██████▄     ██   ─────────    ──████▄
   ████████▄  ▄███████████████████████
  ████████████████████████████████████
   ██████████████████████████████████
    ▀█████████████████████████████▀▀
       ▀▀▀▀▀▀███████████████▀
              ▀▀████████▀▀`
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

// Banner is the startup block: the mascot, the name, and the resolved provider,
// so the first thing on screen says which model is answering before any prompt
// is typed.
func (p Palette) Banner(version, model, provider string) string {
	var b []byte
	b = append(b, p.Bold(mascot)...)
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
