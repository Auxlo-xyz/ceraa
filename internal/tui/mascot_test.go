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

func fullStartup() Startup {
	return Startup{
		Version:  "0.1.0",
		Model:    "gpt-5.6-luna",
		Provider: "openai",
		WorkDir:  "/workspace",
		Effort:   "low",
		Agent:    "forge",
		Tools:    18,
		Session:  "cli-1",
	}
}

func TestBannerCarriesTheWholeStartupState(t *testing.T) {
	got := Plain().Banner(fullStartup(), 80)
	for _, want := range []string{
		"Ceraa", "0.1.0", "gpt-5.6-luna", "openai",
		"/workspace", "low", "forge", "18", "cli-1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("banner is missing %q:\n%s", want, got)
		}
	}
}

// The point of the block is to answer "am I about to talk to the thing I think I
// am". An unset model reported by omitting the row leaves the operator unable to
// tell an unconfigured install from a session that forgot to report.
func TestBannerShowsUnsetRatherThanOmittingTheRow(t *testing.T) {
	got := Plain().Banner(Startup{Version: "0.1.0"}, 80)
	for _, label := range []string{"model", "provider", "effort"} {
		if !strings.Contains(got, label) {
			t.Errorf("banner dropped the %s row instead of showing unset:\n%s", label, got)
		}
	}
	if n := strings.Count(got, unset); n < 3 {
		t.Errorf("expected at least three unset markers, got %d:\n%s", n, got)
	}
}

// Tools is omitted rather than claimed as zero, because a registry that could not
// be built is not a session with no tools.
func TestBannerOmitsToolsWhenThereAreNone(t *testing.T) {
	got := Plain().Banner(Startup{Version: "0.1.0", Model: "gpt"}, 80)
	if strings.Contains(got, "tools") {
		t.Errorf("banner claimed zero tools:\n%s", got)
	}
}

// The face is out of this surface, not shrunk. Nothing may reintroduce a drawing
// to the startup block; a block of block characters is the thing being held.
func TestBannerHasNoArt(t *testing.T) {
	for _, width := range []int{20, 40, 80, 120} {
		got := Plain().Banner(fullStartup(), width)
		if strings.ContainsAny(got, "\u2588\u2580\u2584") {
			t.Errorf("banner drew art at width %d:\n%s", width, got)
		}
	}
}

func TestBannerNeverOverflowsTheTerminal(t *testing.T) {
	for _, width := range []int{20, 40, 44, 60, 100} {
		got := Plain().Banner(fullStartup(), width)
		for i, row := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
			if w := visibleWidth(row); w > width {
				t.Errorf("width %d: banner row %d is %d cells: %q", width, i, w, row)
			}
		}
	}
}

// Every row of the startup block, including the name, starts on one margin. The
// provider row used to come out indented by one, because a right-aligned gutter
// pads a single-row block with a space and there is nothing on its left to line
// up against.
func TestBannerRowsShareOneLeftEdge(t *testing.T) {
	rows := strings.Split(strings.TrimRight(Plain().Banner(fullStartup(), 80), "\n"), "\n")
	if len(rows) < 5 {
		t.Fatalf("expected the name and a block, got %d rows: %q", len(rows), rows)
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
}

// The name is the one accent on screen. It has to be the brightest thing there
// or the block is a list with a title, which is what it looked like before.
func TestBannerNameIsTheAccent(t *testing.T) {
	got := Palette{color: true}.Banner(fullStartup(), 80)
	if !strings.Contains(got, ansiBrightWhite+"Ceraa") {
		t.Errorf("the name is not rendered in the accent:\n%q", got)
	}
	// And it must not leak: the version beside it is context, not identity.
	rest := strings.ReplaceAll(got, ansiBrightWhite+"Ceraa"+ansiReset, "")
	if strings.Contains(rest, ansiBrightWhite) {
		t.Errorf("the accent leaked onto other text:\n%q", rest)
	}
	// Plain output carries no escape at all, so a piped run stays clean text.
	if strings.Contains(Plain().Banner(fullStartup(), 80), "\x1b") {
		t.Error("the plain palette emitted an escape")
	}
}

// A tip that names a command this build does not have teaches a key that does
// nothing, which is worse than showing no tip at all. Every entry is checked
// against the loop's own set, so a renamed command cannot leave a tip behind.
func TestTipsOnlyNameRealCommands(t *testing.T) {
	real := map[string]bool{
		"/help": true, "/status": true, "/sessions": true,
		"/new": true, "/model": true,
	}
	if len(tips) == 0 {
		t.Fatal("there are no tips to show")
	}
	for _, tip := range tips {
		word := strings.Fields(tip)[0]
		if strings.HasPrefix(word, "/") && !real[word] {
			t.Errorf("tip names %q, which is not a command: %q", word, tip)
		}
	}
}

// The tip is chosen from the session id, so consecutive launches differ and the
// same launch is repeatable. A hidden counter would be neither.
func TestTipRotatesOnTheSessionAndIsRepeatable(t *testing.T) {
	byID := map[string]string{}
	for _, id := range []string{"cli-1", "cli-2", "cli-3", "cli-17", "cli-99"} {
		l := &Loop{session: &Session{session: id}}
		got := l.tipText()
		if got == "" {
			t.Fatalf("no tip for session %q", id)
		}
		if prev, seen := byID[id]; seen && prev != got {
			t.Errorf("session %q got two different tips: %q then %q", id, prev, got)
		}
		byID[id] = got
		if len(tips) > 1 && !strings.Contains(got, glyph("tip")) {
			t.Errorf("tip %q is not marked", got)
		}
	}
}

// The startup block is the first thing read, so every row has to survive a narrow
// terminal without being cut through.
func TestStartupBlockStaysReadableAtPhoneWidth(t *testing.T) {
	got := Plain().Banner(fullStartup(), 40)
	for _, row := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if w := visibleWidth(row); w > 40 {
			t.Errorf("row is %d cells: %q", w, row)
		}
	}
	for _, want := range []string{"model", "provider", "cwd", "effort", "tools", "session"} {
		if !strings.Contains(got, want) {
			t.Errorf("the %s row did not survive the width:\n%s", want, got)
		}
	}
}
