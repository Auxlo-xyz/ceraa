package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolvePathFromPrefersExplicitBase pins that a relative path resolves
// against the session workdir rather than the process directory.
//
// A shell tool runs in the session workdir, so a relative path that works in
// bash must work in a tool. Resolving against the process directory instead made
// a file the model had just created look missing.
func TestResolvePathFromPrefersExplicitBase(t *testing.T) {
	base := t.TempDir()
	got, err := ResolvePathFrom(base, "a/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(base, "a/b.txt") {
		t.Fatalf("ResolvePathFrom = %q, want it under %q", got, base)
	}
	absolute, err := ResolvePathFrom(base, "/etc/hosts")
	if err != nil {
		t.Fatal(err)
	}
	if absolute != "/etc/hosts" {
		t.Fatalf("an absolute path must be left alone, got %q", absolute)
	}
	fallback, err := ResolvePathFrom("", "rel.txt")
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	if fallback != filepath.Join(cwd, "rel.txt") {
		t.Fatalf("empty base = %q, want it under the process directory %q", fallback, cwd)
	}
}

// The occurrence count has to survive the trip from the model's JSON to the
// operation spec. JSON numbers arrive as float64, so a parameter that is read as
// an int and compared without narrowing drops silently rather than erroring.
func TestOccurrenceReachesTheSpec(t *testing.T) {
	t.Run("edit_line", func(t *testing.T) {
		spec, err := (&EditTool{}).Translate(map[string]any{
			"path":       "f.txt",
			"old_text":   "x",
			"new_text":   "y",
			"occurrence": float64(3),
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := spec.Args["occurrence"]; got != 3 {
			t.Errorf("occurrence = %v (%T), want 3", got, got)
		}
	})

	t.Run("batch", func(t *testing.T) {
		spec, err := (&BatchEditFileTool{}).Translate(map[string]any{
			"path": "f.txt",
			// []any is the shape Translate sees after a JSON decode, which is
			// what the transport hands it.
			"edits": []any{
				map[string]any{"old_text": "x", "new_text": "y", "occurrence": float64(2)},
				map[string]any{"old_text": "a", "new_text": "b"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		edits, ok := spec.Args["edits"].([]map[string]any)
		if !ok {
			t.Fatalf("edits = %T, want []map[string]any", spec.Args["edits"])
		}
		if got := edits[0]["occurrence"]; got != 2 {
			t.Errorf("edit 0 occurrence = %v, want 2", got)
		}
		// Absent on the second, rather than defaulted to 1, so the operation
		// still enforces uniqueness when the model did not ask for a position.
		if _, present := edits[1]["occurrence"]; present {
			t.Errorf("edit 1 grew an occurrence it was never given: %v", edits[1])
		}
	})
}

// A count of zero or less is not a request for a particular match. Forwarding it
// would turn "old_text must be unique" into "replace whichever match is first",
// which is the silent wrong edit the tool exists to prevent.
func TestANonPositiveOccurrenceIsTreatedAsAbsent(t *testing.T) {
	for _, value := range []any{float64(0), float64(-2), "1", nil, true} {
		spec, err := (&EditTool{}).Translate(map[string]any{
			"path": "f.txt", "old_text": "x", "new_text": "y", "occurrence": value,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, present := spec.Args["occurrence"]; present {
			t.Errorf("occurrence %v was forwarded, want it dropped", value)
		}
	}
}

// The schema is what the model actually reads. Without occurrence declared, a
// model following the tool description has no argument to pass.
func TestTheSchemasOfferOccurrence(t *testing.T) {
	// Matched as a JSON key, not as a word. The descriptions legitimately talk
	// about occurrences, and a substring check on the whole schema would pass
	// while the argument itself was never declared.
	const key = `"occurrence":`
	for _, tool := range []struct {
		name   string
		schema map[string]any
	}{
		{"edit_line", (&EditTool{}).Schema()},
		{"batch_edit_file", (&BatchEditFileTool{}).Schema()},
	} {
		encoded, err := json.Marshal(tool.schema)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), key) {
			t.Errorf("%s does not declare occurrence in its schema: %s", tool.name, encoded)
		}
	}
}

// offset and limit have to survive the trip from the model's JSON to the
// operation spec, or read silently returns the whole file and the model thinks it
// asked for a window.
func TestRangedReadReachesTheSpec(t *testing.T) {
	spec, err := (&ReadTool{}).Translate(map[string]any{
		"path": "f.txt", "offset": float64(400), "limit": float64(60),
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Args["offset"] != 400 || spec.Args["limit"] != 60 {
		t.Errorf("spec = %+v, want offset 400 and limit 60", spec.Args)
	}
}

// A read with no range must keep the spec shape it always had, so a session
// written before this replays unchanged and nothing narrows by default.
func TestAPlainReadCarriesNoRange(t *testing.T) {
	spec, err := (&ReadTool{}).Translate(map[string]any{"path": "f.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Args) != 1 {
		t.Errorf("args = %+v, want only path", spec.Args)
	}
}

// A zero, negative or fractional offset would become a slice bound that reads
// nothing or panics. Dropping it is worse than a failed call and far better than
// a wrong one.
func TestAnUnusableRangeIsTreatedAsAbsent(t *testing.T) {
	for _, bad := range []any{float64(0), float64(-5), 1.5, "10", true, nil} {
		spec, err := (&ReadTool{}).Translate(map[string]any{"path": "f.txt", "offset": bad, "limit": bad})
		if err != nil {
			t.Fatal(err)
		}
		if _, present := spec.Args["offset"]; present {
			t.Errorf("offset %v was forwarded, want it dropped", bad)
		}
	}
}
