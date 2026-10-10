package instruction

import (
	"reflect"
	"strings"
	"testing"

	"ceraa/internal/testsupport"
)

// TestMain keeps this package's logs out of the live install.
//
// store.go warns when it throws a cache away, so an unguarded suite writes those
// lines into the running bot's bridge.log. The redirection has to be here rather
// than at each call site because Go runs every package's tests in its own
// binary and one package's environment is invisible to its siblings.
func TestMain(m *testing.M) { testsupport.Main(m) }

// FuzzCompileNeverPanics feeds arbitrary bytes to the compiler.
//
// A parser is exactly what a fuzz target is for: the segmentation walks a line
// at a time with a fence state, a comment state, a table state, a heading stack
// and an open builder, and no table of inputs covers the interleavings. The
// seeds are the constructs that carry state between lines, because those are the
// ones a malformed file can leave half-open at EOF.
func FuzzCompileNeverPanics(f *testing.F) {
	seeds := []string{
		"",
		"\n",
		"# Heading\n\n- A rule\n",
		"- A rule\n  continued here\n",
		"```bash\nnot a rule\n```\n",
		"```\nunclosed\n- swallowed\n",
		"~~~~\nunclosed\n",
		"<!-- comment\nstill a comment\n--> \n- rule\n",
		"<!-- never closed\n",
		"| a | b |\n|---|---|\n| c | d |\n",
		"| a | b |\n| c | d |\n",
		"> - quoted rule\n>> deeper\n",
		"---\n***\n___\n",
		"[mandatory]\n[applies=]\n[tools=,,]\n",
		"- [applies=**/*.go] [triggers=error] rule\n",
		strings.Repeat("- long rule ", 200) + "\n",
		"#\n# \n####### seven\n",
		"1. ordered\n2) also ordered\n",
		"\tindented code\n    four spaces\n",
		strings.Repeat("|", 500),
		strings.Repeat("#", 700) + " heading\n",
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Add([]byte("# Heading\r\n\r\n- CRLF rule\r\n"))
	f.Add([]byte("\xff\xfe\x00binary\x00"))

	f.Fuzz(func(t *testing.T, data []byte) {
		snap, diags := Compile("AGENTS.md", data)

		// The invariants that must hold for every input, because a compiler that
		// panics is not the only way to be wrong: one that returns a rule whose
		// id is not its own hash returns a delivered set that suppresses the
		// wrong sentences.
		for _, item := range snap.Instructions {
			if len(item.Text) == 0 {
				t.Fatalf("an empty rule was emitted with id %q", item.ID)
			}
			if len(item.Text) > MaxInstructionBytes {
				t.Fatalf("a rule of %d bytes survived the %d cap", len(item.Text), MaxInstructionBytes)
			}
			if item.Line < 1 || item.EndLine < item.Line {
				t.Fatalf("rule %q has the impossible extent %d-%d", item.ID, item.Line, item.EndLine)
			}
			if !item.Oversized && len(item.Text) == MaxInstructionBytes {
				t.Fatalf("a rule at exactly the cap is not marked oversized: %q", item.Text)
			}
			base := "i" + item.Hash[:24]
			if item.DuplicateOf == "" && string(item.ID) != base {
				t.Fatalf("id %q does not match hash %q", item.ID, item.Hash)
			}
			if item.DuplicateOf != "" && string(item.DuplicateOf) != base {
				t.Fatalf("a repeat points at %q, not its own hash %q", item.DuplicateOf, item.Hash)
			}
		}
		// Every finding names a file and a line inside it.
		for _, item := range diags {
			if item.Source != "AGENTS.md" || item.Line < 1 || item.EndLine < item.Line {
				t.Fatalf("diagnostic %+v does not name a location in the file", item)
			}
		}

		// And it is pure: the same bytes give the same value, every time.
		again, _ := Compile("AGENTS.md", data)
		if !reflect.DeepEqual(snap.Instructions, again.Instructions) {
			t.Fatal("compiling the same bytes twice gave two different results")
		}
	})
}
