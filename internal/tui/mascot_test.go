package tui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMascotIsNotEmpty(t *testing.T) {
	if strings.TrimSpace(mascot) == "" {
		t.Fatal("the mascot is empty")
	}
}

func TestMascotFitsAReasonableTerminal(t *testing.T) {
	// The mascot is drawn for a terminal, so a row wider than a narrow terminal
	// wraps and corrupts everything printed after it. Runes, not bytes: the block
	// glyphs are three-byte UTF-8.
	const minTerminal = 44
	for i, r := range strings.Split(strings.TrimRight(mascot, "\n"), "\n") {
		if cols := utf8.RuneCountInString(r); cols > minTerminal {
			t.Errorf("row %d is %d columns, wider than %d: %q", i, cols, minTerminal, r)
		}
	}
}

func TestMascotHasBothEyes(t *testing.T) {
	// The wink and the oval eye are the character. A drawing that loses either is
	// a different face, so this asserts both are present rather than asserting a
	// row count that a redesign would break.
	//
	// The wink is drawn as a flat line and the open eye is a gap in the white
	// head, so the eye is asserted on the gap existing on the feature rows rather
	// than on any particular glyph.
	if !strings.Contains(mascot, "─") {
		t.Error("the mascot has no flat wink")
	}
	// The open eye is the tall white void left of centre. It shows as a run of
	// spaces bounded by block glyphs on the feature rows.
	found := false
	for _, row := range strings.Split(mascot, "\n") {
		if !strings.Contains(row, "█") {
			continue
		}
		// A run of three or more spaces inside a row that is otherwise solid is
		// the eye void.
		if strings.Contains(row, "   █") || strings.Contains(row, "█    ") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("the mascot has no open eye void:\n%s", mascot)
	}
}

// State still drives the status line, so it must keep naming itself even though
// the mascot no longer varies by it.
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
