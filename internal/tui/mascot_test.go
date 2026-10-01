package tui

import (
	"strings"
	"testing"
)

// The state drives the status line, so it must keep naming itself.
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

// The face is out of this surface, not shrunk. Nothing may reintroduce a drawing
// to the startup block; a block of block characters is the thing being held.
func TestBannerHasNoArt(t *testing.T) {
	for _, width := range []int{20, 40, 80, 120} {
		got := Plain().Banner("0.1.0", "gpt-5.5", "openai", width)
		if strings.ContainsAny(got, "▄█▀") {
			t.Errorf("banner drew art at width %d:\n%s", width, got)
		}
	}
}

// The block is still fitted: dropping the drawing did not take the width with it.
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

// The three lines of the startup block, measured. The provider row used to come
// out indented by one, because a right-aligned gutter pads a single-row block by
// a space and there is nothing on the left to line up with, so the block started
// half a cell off its own margin.
func TestBannerRowsShareOneLeftEdge(t *testing.T) {
	got := Plain().Banner("0.1.0", "", "default", 80)
	rows := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(rows) != 2 {
		t.Fatalf("expected the name and the provider, got %d rows: %q", len(rows), rows)
	}
	edge := len(rows[0]) - len(strings.TrimLeft(rows[0], " "))
	for i, r := range rows {
		if got := len(r) - len(strings.TrimLeft(r, " ")); got != edge {
			t.Errorf("row %d starts at column %d, want the shared %d: %q", i, got, edge, r)
		}
	}
	if rows[0] != "  Ceraa  0.1.0" {
		t.Errorf("name row is %q", rows[0])
	}
	if rows[1] != "  provider  default" {
		t.Errorf("provider row is %q", rows[1])
	}
}
