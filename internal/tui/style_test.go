package tui

import (
	"strings"
	"testing"
)

func TestWrapTextKeepsShortLinesWhole(t *testing.T) {
	got := wrapText("the quick brown fox", 40)
	want := []string{"the quick brown fox"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("wrapText = %q, want %q", got, want)
	}
}

func TestWrapTextBreaksOnWordBoundaries(t *testing.T) {
	got := wrapText("alpha beta gamma delta", 11)
	// Every line must fit, and no word may be split.
	for _, line := range got {
		if len(line) > 11 {
			t.Fatalf("line %q exceeds the limit", line)
		}
	}
	joined := strings.Join(got, " ")
	if joined != "alpha beta gamma delta" {
		t.Fatalf("wrapping lost or reordered words: %q", joined)
	}
}

func TestWrapTextBreaksAnOverlongWord(t *testing.T) {
	long := strings.Repeat("x", 25)
	got := wrapText(long, 10)
	if len(got) < 3 {
		t.Fatalf("expected an overlong word to be split, got %q", got)
	}
	for _, line := range got {
		if len(line) > 10 {
			t.Fatalf("line %q exceeds the limit", line)
		}
	}
	if strings.Join(got, "") != long {
		t.Fatalf("splitting the word lost characters")
	}
}

func TestWrapTextPreservesBlankLines(t *testing.T) {
	got := wrapText("a\n\nb", 40)
	if len(got) != 3 || got[1] != "" {
		t.Fatalf("blank line not preserved: %q", got)
	}
}

func TestWrapTextClampsATinyWidth(t *testing.T) {
	// A width below the floor must not produce a zero-length split loop.
	got := wrapText("hello world", 1)
	if len(got) == 0 {
		t.Fatal("expected output for a tiny width")
	}
}

func TestPalettePlainEmitsNoEscapes(t *testing.T) {
	p := Plain()
	if p.Color() {
		t.Fatal("Plain() reports colour")
	}
	for name, got := range map[string]string{
		"Bold":      p.Bold("x"),
		"Dim":       p.Dim("x"),
		"Reverse":   p.Reverse("x"),
		"Underline": p.Underline("x"),
		"BoldOn":    p.BoldOn(),
		"DimOn":     p.DimOn(),
		"Reset":     p.Reset(),
		"Invert":    p.Invert(),
	} {
		if strings.Contains(got, "\x1b") {
			t.Errorf("%s emitted an escape with colour off: %q", name, got)
		}
	}
}

func TestPaletteStyledWrapsAndResets(t *testing.T) {
	p := Palette{color: true}
	got := p.Bold("x")
	if !strings.HasPrefix(got, "\x1b[1m") || !strings.HasSuffix(got, ansiReset) {
		t.Fatalf("Bold did not wrap and reset: %q", got)
	}
}

func TestPaletteStyledOnEmptyIsEmpty(t *testing.T) {
	p := Palette{color: true}
	if got := p.Bold(""); got != "" {
		t.Fatalf("an empty string must not gain escapes, got %q", got)
	}
}

func TestSpinnerWrapsAndSurvivesNegatives(t *testing.T) {
	n := len(spinnerFrames)
	if Spinner(0) != spinnerFrames[0] {
		t.Fatal("tick 0 is not the first frame")
	}
	if Spinner(n) != spinnerFrames[0] {
		t.Fatal("the frame cycle does not wrap")
	}
	// A clock that steps backwards must not index out of range.
	if got := Spinner(-1); got != spinnerFrames[n-1] {
		t.Fatalf("negative tick produced %q", got)
	}
}
