package telegram

import "testing"

// The table faults below were transcribed from live transcripts: the
// delimiter row above the header is exactly what the model wrote for the two
// tables visible in chat on the day the healing was built.

func TestFixTablesMovesTheDelimiterUnderTheHeader(t *testing.T) {
	dirty := "|---|---|---|\n| Surface | File | The label |\n| Channel audit header | `features/` | A note strip |"
	want := "| Surface | File | The label |\n|---|---|---|\n| Channel audit header | `features/` | A note strip |"
	if got := fixTables(dirty); got != want {
		t.Fatalf("fixTables:\n%q\nwant:\n%q", got, want)
	}
}

func TestFixTablesInsertsAMissingDelimiter(t *testing.T) {
	dirty := "| File | Change | Why |\n| `package.json:26` | New script | The chain |"
	want := "| File | Change | Why |\n|---|---|---|\n| `package.json:26` | New script | The chain |"
	if got := fixTables(dirty); got != want {
		t.Fatalf("fixTables:\n%q\nwant:\n%q", got, want)
	}
}

func TestFixTablesPadsRaggedRowsInPlace(t *testing.T) {
	dirty := "| A | B | C |\n| one | two |\n| x | y | z |"
	want := "| A | B | C |\n|---|---|---|\n| one | two | |\n| x | y | z |"
	if got := fixTables(dirty); got != want {
		t.Fatalf("fixTables:\n%q\nwant:\n%q", got, want)
	}
}

func TestFixTablesKeepsAlignmentColons(t *testing.T) {
	dirty := "|:---|:---:|\n| Left | Center |"
	want := "| Left | Center |\n|:---|:---:|"
	if got := fixTables(dirty); got != want {
		t.Fatalf("fixTables:\n%q\nwant:\n%q", got, want)
	}
}

func TestFixTablesSpawnsNothingAroundWellFormedTables(t *testing.T) {
	well := "Intro\n\n| Metric | Value |\n|:-------|------:|\n| Speed | **42** ms |\n| Status | ready |\n\ntail"
	if got := fixTables(well); got != well {
		t.Fatalf("fixTables changed a well-formed table:\n%q", got)
	}
}

func TestFixTablesLeavesProsePipesAlone(t *testing.T) {
	prose := "an operator pipe like `a | b` on one line,\nand a second line that starts with a quote, not a pipe"
	if got := fixTables(prose); got != prose {
		t.Fatalf("fixTables changed prose: %q", got)
	}
}

func TestFixTablesSkipsCodeFences(t *testing.T) {
	block := "```\n|---|---|\n| A | B |\n```"
	if got := fixTables(block); got != block {
		t.Fatalf("fixTables healed inside a fence: %q", got)
	}
}

func TestFixTablesHealsAroundAFence(t *testing.T) {
	dirty := "before\n\n|---|---|\n| A | B |\n\n```\n| keep | raw |\n```\n\nafter"
	want := "before\n\n| A | B |\n|---|---|\n\n```\n| keep | raw |\n```\n\nafter"
	if got := fixTables(dirty); got != want {
		t.Fatalf("fixTables:\n%q\nwant:\n%q", got, want)
	}
}

func TestFixTablesRebuildsRowsByteForByte(t *testing.T) {
	// An escaped pipe inside a cell must survive a pad: the row is split on
	// raw pipes and rebuilt with the same delimiter.
	dirty := "| A | B |\n| data `x \\| y` more |"
	want := "| A | B |\n|---|---|\n| data `x \\| y` more |"
	if got := fixTables(dirty); got != want {
		t.Fatalf("fixTables:\n%q\nwant:\n%q", got, want)
	}
}

func TestFixTablesLeavesAShortMessageAlone(t *testing.T) {
	for _, md := range []string{"", "no pipes at all", "| one lonely line |"} {
		if got := fixTables(md); got != md {
			t.Fatalf("fixTables changed %q into %q", md, got)
		}
	}
}
