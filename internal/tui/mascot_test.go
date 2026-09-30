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
	// The wink and the open eye are the character, and a drawing that loses
	// either is a different face.
	//
	// The mascot is drawn in the foreground, so both features are absences of
	// ink. The open eye is the wide void left of centre; the wink is the pair of
	// shorter voids to its right on one row. Asserting on the specific voids is
	// what makes this catch a redesign that loses a feature, which asserting on
	// row counts would not.
	rows := strings.Split(mascot, "\n")

	// The open eye: the widest run of spaces inside a row that also holds ink.
	widestEye := 0
	// The wink: two runs of the same length, well short of the eye, on one row.
	winkLen, winkFound := 0, false
	for _, r := range rows {
		if !strings.ContainsAny(r, "\u2588\u2580\u2584") {
			continue
		}
		var runs []int
		for _, run := range spaceRuns(r) {
			runs = append(runs, run)
		}
		for _, run := range runs {
			if run > widestEye {
				widestEye = run
			}
		}
		// A wink is a matched pair of short voids, not a stray one-space gap.
		short := []int{}
		for _, run := range runs {
			if run >= 2 && run < widestEyeFor(runs) {
				short = append(short, run)
			}
		}
		for i := 0; i+1 < len(short); i++ {
			if short[i] == short[i+1] {
				winkFound, winkLen = true, short[i]
			}
		}
	}
	if widestEye < 6 {
		t.Errorf("the open eye is too small (widest void %d, want at least 6):\n%s", widestEye, mascot)
	}
	if !winkFound {
		t.Errorf("no wink found: no matched pair of short voids on any row:\n%s", mascot)
	}
	_ = winkLen
}

// widestEyeFor returns the longest space run in a row, which is the open eye.
func widestEyeFor(runs []int) int {
	best := 0
	for _, r := range runs {
		if r > best {
			best = r
		}
	}
	return best
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

// spaceRuns returns the length of every run of spaces in a row.
func spaceRuns(row string) []int {
	var out []int
	n := 0
	for _, r := range row {
		if r == ' ' {
			n++
			continue
		}
		if n > 0 {
			out = append(out, n)
			n = 0
		}
	}
	if n > 0 {
		out = append(out, n)
	}
	return out
}
