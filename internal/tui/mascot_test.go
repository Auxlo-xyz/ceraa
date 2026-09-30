package tui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMascotIsNotEmpty(t *testing.T) {
	if strings.TrimSpace(mascotMark) == "" {
		t.Fatal("the mascot is empty")
	}
}

func TestMascotFitsAReasonableTerminal(t *testing.T) {
	// The mark stands over the startup block, so a row wider than a phone wraps
	// and corrupts everything printed after it. Runes, not bytes: the block
	// glyphs are three-byte UTF-8.
	const minTerminal = 24
	for i, r := range strings.Split(strings.TrimRight(mascotMark, "\n"), "\n") {
		if cols := utf8.RuneCountInString(r); cols > minTerminal {
			t.Errorf("row %d is %d columns, wider than %d: %q", i, cols, minTerminal, r)
		}
	}
}

func TestMascotHasBothEyes(t *testing.T) {
	// The wink and the open eye are the character, and a drawing that loses
	// either is a different face.
	//
	// The mark is drawn in the foreground, so both features are absences of
	// ink. The open eye is the wide void left of centre; the wink is the pair of
	// shorter voids to its right on one row. Asserting on the specific voids is
	// what makes this catch a redraw that loses a feature, which asserting on
	// row counts would not. Rows are measured past their indent, so the padding
	// that stands the mark off the margin cannot stand in for an eye.
	rows := strings.Split(mascotMark, "\n")

	// The open eye: the widest interior run of spaces in a row that holds ink.
	widestEye := 0
	// The wink: two runs of the same length, well short of the eye, on one row.
	winkFound := false
	for _, r := range rows {
		r = strings.TrimLeft(r, " ")
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
				winkFound = true
			}
		}
	}
	if widestEye < 4 {
		t.Errorf("the open eye is too small (widest void %d, want at least 4):\n%s", widestEye, mascotMark)
	}
	if !winkFound {
		t.Errorf("no wink found: no matched pair of short voids on any row:\n%s", mascotMark)
	}
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
	got := p.Banner("0.1.0", "gpt-5.5", "openai", 80)
	for _, want := range []string{"Ceraa", "0.1.0", "gpt-5.5", "openai"} {
		if !strings.Contains(got, want) {
			t.Errorf("banner is missing %q:\n%s", want, got)
		}
	}
}

func TestBannerOmitsAbsentFields(t *testing.T) {
	got := Plain().Banner("0.1.0", "", "", 80)
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

// fitArt gives up indent before it gives up art, and the two tests below are the
// whole contract: shifting is allowed, cutting is not.
func TestFitArtSpendsItsIndentBeforeItsArt(t *testing.T) {
	got, ok := fitArt(mascotMark, 20)
	if !ok {
		t.Fatal("fitArt(20) refused art that has 2 cells of indent to spend")
	}
	want := strings.Split(mascotMark, "\n")
	have := strings.Split(got, "\n")
	if len(have) != len(want) {
		t.Fatalf("fitArt changed the row count: %d, want %d", len(have), len(want))
	}
	for i := range want {
		if w := runeLen(have[i]); w > 20 {
			t.Errorf("row %d is %d cells: %q", i, w, have[i])
		}
		// The row's own shape is identical; only the padding in front moved.
		if strings.TrimLeft(have[i], " ") != strings.TrimLeft(want[i], " ") {
			t.Errorf("row %d lost art:\n have %q\nwant %q", i, have[i], want[i])
		}
		if runeLen(have[i]) > runeLen(want[i]) {
			t.Errorf("row %d gained width: %d, want at most %d", i, runeLen(have[i]), runeLen(want[i]))
		}
	}
}

func TestFitArtLeavesArtAloneWhenItFits(t *testing.T) {
	for _, width := range []int{22, 60, 120} {
		got, ok := fitArt(mascotMark, width)
		if !ok {
			t.Errorf("fitArt(%d) dropped art that fits", width)
			continue
		}
		if got != mascotMark {
			t.Errorf("fitArt(%d) rewrote art it did not need to touch", width)
		}
	}
}

// Below the width that indent can cover there is no honest way to draw the
// logo, so it is not drawn. A mark cut through its edge is worse than none.
func TestFitArtDropsArtItCannotDraw(t *testing.T) {
	if _, ok := fitArt(mascotMark, 19); ok {
		t.Error("fitArt(19) drew art it could not fit")
	}
	if _, ok := fitArt(mascotMark, 0); ok {
		t.Error("fitArt(0) drew art for an unknown width")
	}
}

func TestBannerNeverOverflowsTheTerminal(t *testing.T) {
	for _, width := range []int{20, 40, 44, 60, 100} {
		got := Plain().Banner("0.1.0", "gpt-5.5", "openai", width)
		for i, row := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
			if w := visibleWidth(row); w > width {
				t.Errorf("width %d: banner row %d is %d cells: %q", width, i, w, row)
			}
		}
	}
}

func TestBannerDropsTheArtRatherThanCuttingIt(t *testing.T) {
	// Nineteen is the exact width where the mark has to go but the words still
	// fit: the mark is 22 wide with 2 of indent to spend, and with only the
	// provider row set the longest text row is 19.
	got := Plain().Banner("0.1.0", "", "openai", 19)
	if strings.ContainsAny(got, "▄█▀") {
		t.Errorf("art was drawn on a 19-column terminal:\n%s", got)
	}
	for _, want := range []string{"Ceraa", "openai"} {
		if !strings.Contains(got, want) {
			t.Errorf("the banner lost %q when the art went:\n%s", want, got)
		}
	}
}
