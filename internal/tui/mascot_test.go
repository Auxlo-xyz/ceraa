package tui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMascotFacesAreDistinctPerState(t *testing.T) {
	seen := map[string]State{}
	for _, s := range []State{StateIdle, StateThinking, StateWorking, StateDone, StateError} {
		face := s.face()
		if strings.TrimSpace(face) == "" {
			t.Fatalf("state %s has an empty face", s)
		}
		if prev, dup := seen[face]; dup {
			t.Fatalf("states %s and %s share a face", prev, s)
		}
		seen[face] = s
	}
}

func TestMascotFacesHaveConsistentShape(t *testing.T) {
	// Every state must be the same silhouette, so the banner does not jump between
	// shapes as a run changes state. The rows are compared, not the glyphs.
	want := strings.Count(mascotIdle, "\n") + 1
	for _, s := range []State{StateIdle, StateThinking, StateWorking, StateDone, StateError} {
		if got := strings.Count(s.face(), "\n") + 1; got != want {
			t.Errorf("state %s has %d rows, idle has %d", s, got, want)
		}
	}
}

func TestMascotFitsAReasonableTerminal(t *testing.T) {
	// The mascot is drawn for a terminal, so a row that is wider than a narrow
	// terminal wraps and corrupts everything printed after it. Runes, not bytes:
	// the block glyphs are three-byte UTF-8.
	const minTerminal = 44
	for _, st := range []State{StateIdle, StateThinking, StateWorking, StateDone, StateError} {
		for i, r := range strings.Split(strings.TrimRight(st.face(), "\n"), "\n") {
			if cols := utf8.RuneCountInString(r); cols > minTerminal {
				t.Errorf("state %s row %d is %d columns, wider than %d: %q", st, i, cols, minTerminal, r)
			}
		}
	}
}

func TestMascotRowCountIsStable(t *testing.T) {
	// A state with a different height makes the banner jump as a run changes
	// state, so every face must have the same number of rows as the idle one.
	want := len(strings.Split(strings.TrimRight(mascotIdle, "\n"), "\n"))
	for _, st := range []State{StateIdle, StateThinking, StateWorking, StateDone, StateError} {
		got := len(strings.Split(strings.TrimRight(st.face(), "\n"), "\n"))
		if got != want {
			t.Errorf("state %s has %d rows, idle has %d", st, got, want)
		}
	}
}

func TestMascotOutlineIsIdenticalAcrossStates(t *testing.T) {
	// Only the features move between states. The cloud outline, which is rows 0
	// and 9, must be byte-identical in every state, or the face is a different
	// character in each state rather than one face in several moods.
	states := []State{StateIdle, StateThinking, StateWorking, StateDone, StateError}
	outlineOf := func(st State) []string {
		rows := strings.Split(strings.TrimRight(st.face(), "\n"), "\n")
		return []string{rows[0], rows[len(rows)-1]}
	}
	want := outlineOf(StateIdle)
	for _, st := range states[1:] {
		got := outlineOf(st)
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("state %s outline row %d = %q, idle has %q", st, i, got[i], want[i])
			}
		}
	}
}

func TestMascotFeatureRowsDifferBetweenStates(t *testing.T) {
	// Each state must actually change something, or five states are one state
	// drawn five times and the operator learns nothing from the mascot.
	faces := map[string]State{}
	for _, st := range []State{StateIdle, StateThinking, StateWorking, StateDone, StateError} {
		faces[st.face()] = st
	}
	if len(faces) != 5 {
		t.Errorf("%d distinct faces for 5 states, want 5", len(faces))
	}
}

func TestMascotFacesFitATerminalWidth(t *testing.T) {
	// A mascot wider than a narrow terminal wraps and destroys the layout of
	// everything after it, so the width has to stay inside a common minimum.
	const minTerminal = 40
	for _, st := range []State{StateIdle, StateThinking, StateWorking, StateDone, StateError} {
		for i, r := range strings.Split(strings.TrimRight(st.face(), "\n"), "\n") {
			// Runes, not bytes: the block glyphs are three-byte UTF-8 and a
			// byte count would report every row as three times too wide.
			if cols := utf8.RuneCountInString(r); cols > minTerminal {
				t.Errorf("state %s row %d is %d columns, wider than a %d column terminal: %q",
					st, i, cols, minTerminal, r)
			}
		}
	}
}

func TestStateStringNamesEveryState(t *testing.T) {
	for _, tc := range []struct {
		state State
		want  string
	}{
		{StateIdle, "idle"},
		{StateThinking, "thinking"},
		{StateWorking, "working"},
		{StateDone, "done"},
		{StateError, "error"},
	} {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("State(%d).String() = %q, want %q", tc.state, got, tc.want)
		}
	}
	if got := State(99).String(); got != "unknown" {
		t.Errorf("an unknown state named itself %q", got)
	}
}

func TestBannerCarriesModelAndProvider(t *testing.T) {
	p := Plain()
	got := p.Banner("0.1.0", "gpt-5.5", "openai")
	for _, want := range []string{"Ceraa", "0.1.0", "gpt-5.5", "openai"} {
		if !strings.Contains(got, want) {
			t.Errorf("banner is missing %q:\n%s", want, got)
		}
	}
}

func TestBannerOmitsAbsentFields(t *testing.T) {
	got := Plain().Banner("0.1.0", "", "")
	if strings.Contains(got, "model") {
		t.Errorf("banner printed a model line for an empty model:\n%s", got)
	}
	if strings.Contains(got, "provider") {
		t.Errorf("banner printed a provider line for an empty provider:\n%s", got)
	}
}
