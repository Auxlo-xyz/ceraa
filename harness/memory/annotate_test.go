package memory

import (
	"strings"
	"testing"
)

// Annotate is the only renderer between a selected memory and the tool result
// the model reads, so its contract is pinned here: nothing in, nothing out; a
// note that fits goes out whole with its markup; a note that does not fit is
// capped with a notice that admits it rather than trimmed silently.
func TestAnnotateRendersNothingForNothing(t *testing.T) {
	if got := Annotate([]string{"/srv/api"}, nil, nil); got != "" {
		t.Errorf("empty selection rendered %q", got)
	}
}

func TestAnnotateCarriesTheWholeNoteInsideItsMarkup(t *testing.T) {
	note := Annotate([]string{"/srv/api/main.go"},
		[]Brief{{ID: "a", Applies: "/srv/api", Content: "the backend pins jdk 21"}},
		[]Brief{{ID: "b", Content: "the user prefers dark mode"}})
	if !strings.Contains(note, "the backend pins jdk 21") || !strings.Contains(note, "dark mode") {
		t.Fatalf("the note lost a selection: %q", note)
	}
	if !strings.HasPrefix(note, "\n\n<sidekick") || !strings.HasSuffix(strings.TrimSpace(note), "</sidekick>") {
		t.Errorf("the note is not a whole sidekick block: %q", note)
	}
}

func TestAnnotateCapsTheNoteAndAdmitsIt(t *testing.T) {
	var general []Brief
	for i := 0; i < 40; i++ {
		general = append(general, Brief{ID: string(rune('a' + i)), Content: strings.Repeat("standing preference ", 8)})
	}
	note := Annotate([]string{"/srv/api/main.go"}, nil, general)
	if len(note) > SidekickBlockBytes {
		t.Fatalf("the note is %d bytes, over the %d ceiling", len(note), SidekickBlockBytes)
	}
	if !strings.Contains(note, "not shown") {
		t.Errorf("a capped note does not admit what it left out: %q", note)
	}
	if !strings.HasSuffix(strings.TrimSpace(note), "</sidekick>") {
		t.Errorf("a capped note was cut inside its own markup: %q", note)
	}
}
