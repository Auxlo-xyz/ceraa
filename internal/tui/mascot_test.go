package tui

import (
	"strings"
	"testing"
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

func TestMascotFacesStayWithinOneWidthBand(t *testing.T) {
	// A rounded face is legitimately ragged: the crown is narrower than the
	// middle. What would look broken is a row that escapes the band the other
	// states share, so the widest and narrowest rows are compared against a fixed
	// band rather than against each other.
	const minWidth, maxWidth = 15, 23
	for _, s := range []State{StateIdle, StateThinking, StateWorking, StateDone, StateError} {
		rows := strings.Split(strings.TrimRight(s.face(), "\n"), "\n")
		for i, r := range rows {
			if len(r) < minWidth || len(r) > maxWidth {
				t.Errorf("state %s row %d is %d wide, outside [%d,%d]: %q",
					s, i, len(r), minWidth, maxWidth, r)
			}
		}
	}
}

func TestMascotBodyRowsShareAWidth(t *testing.T) {
	// Rows 1 to 4 are the face proper: the eye row, both cheeks and the mouth
	// row. They must be the same width in every state, so only the features move
	// and the head does not. A tie between the two widest rows is not a defect,
	// which is why this compares the set of body widths rather than picking one
	// "widest" row and comparing indices.
	for _, s := range []State{StateIdle, StateThinking, StateWorking, StateDone, StateError} {
		got := bodyWidths(s.face())
		want := bodyWidths(mascotIdle)
		if len(got) != len(want) {
			t.Fatalf("state %s has %d body rows, idle has %d", s, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("state %s body row %d is %d wide, idle is %d", s, i, got[i], want[i])
			}
		}
	}
}

// bodyWidths measures rows 1 through 4, the face below the crown.
func bodyWidths(face string) []int {
	rows := strings.Split(strings.TrimRight(face, "\n"), "\n")
	var out []int
	for _, r := range rows[1:] {
		out = append(out, len(r))
	}
	return out
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
