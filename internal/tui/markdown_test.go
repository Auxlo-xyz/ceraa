package tui

import (
	"strings"
	"testing"
)

// A row has to begin with a pipe, and that is the only thing a partial line can
// be judged on. Judging a continuation chunk on its own is what put a table row
// back to front: the chunk after `| **bash` does not start with a pipe, so it was
// written out ahead of the prefix already held.
func TestTableIsReassembledAcrossArbitraryDeltaBoundaries(t *testing.T) {
	tbl := "Here are the tools:\n\n" +
		"| Tool | Purpose |\n" +
		"|-------|---------|\n" +
		"| **bash** | Execute shell commands |\n" +
		"| **task** | Delegate work |\n" +
		"\nThat is all."
	for _, size := range []int{1, 2, 3, 5, 7, 11, 13, 64} {
		var b strings.Builder
		r := NewRenderer(&b, Plain())
		r.cols = 100
		for i := 0; i < len(tbl); i += size {
			end := i + size
			if end > len(tbl) {
				end = len(tbl)
			}
			r.Content(tbl[i:end])
		}
		r.closeStream()
		got := b.String()
		for _, want := range []string{"bash", "Execute shell commands", "task", "Delegate work", "That is all."} {
			if !strings.Contains(got, want) {
				t.Errorf("delta size %d lost %q:\n%s", size, want, got)
			}
		}
		// Nothing may print between the lead-in and the table, or after it.
		if strings.Index(got, "That is all.") < strings.Index(got, "bash") {
			t.Errorf("delta size %d printed the table after the prose that followed it:\n%s", size, got)
		}
		if strings.Contains(got, "**") {
			t.Errorf("delta size %d left emphasis markers in: %q", size, got)
		}
	}
}

// The point of holding a row at all. Without this the pipes are the most legible
// thing on a narrow screen and every cell is cut in half.
func TestNarrowTerminalStacksATableInsteadOfSqueezingIt(t *testing.T) {
	tbl := "| Tool | Purpose |\n|-------|---------|\n" +
		"| **bash** | Execute shell commands (default for running/inspecting, up to 900s) |\n" +
		"\n"
	var b strings.Builder
	r := NewRenderer(&b, Plain())
	r.cols = 44
	r.Content(tbl)
	r.closeStream()
	got := b.String()
	if strings.Contains(got, "|") {
		t.Errorf("pipes survived into a narrow render:\n%s", got)
	}
	for _, l := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if len(l) > 44 {
			t.Errorf("row is %d cells: %q", len(l), l)
		}
	}
	// The key on its own line, the description indented under it. That is what
	// makes a two column table readable at forty cells.
	if !strings.Contains(got, "  bash\n") {
		t.Errorf("the tool name is not on its own line:\n%s", got)
	}
	if !strings.Contains(got, "    Execute shell") {
		t.Errorf("the description is not indented under the name:\n%s", got)
	}
}

// A wide terminal gets the columns, because a table that fits is worth showing as
// a table.
func TestWideTerminalKeepsTheColumns(t *testing.T) {
	tbl := "| Tool | Purpose |\n|-------|---------|\n" +
		"| **bash** | Execute shell |\n| **task** | Delegate work |\n\n"
	var b strings.Builder
	r := NewRenderer(&b, Plain())
	r.cols = 100
	r.Content(tbl)
	r.closeStream()
	// The answer opens with a blank line to leave the prompt, so the rows start
	// after it.
	rows := strings.Split(strings.TrimRight(strings.TrimLeft(b.String(), "\n"), "\n"), "\n")
	if len(rows) != 3 {
		t.Fatalf("expected header and two rows, got %d: %q", len(rows), rows)
	}
	// Every value starts at the same column, which is the whole point of keeping
	// the columns at all.
	for i, want := range []string{"Execute shell", "Delegate work"} {
		col := strings.Index(rows[i+1], want)
		if col < 0 {
			t.Fatalf("row %d is missing its value: %q", i, rows[i+1])
		}
		if i > 0 && col != strings.Index(rows[1], "Execute shell") {
			t.Errorf("row %d value starts at column %d, want the shared %d: %q",
				i, col, strings.Index(rows[1], "Execute shell"), rows[i+1])
		}
	}
}

// Prose is held to the end of its line, and rendered the moment it completes.
// The bound is a line and not the answer: a paragraph arrives as it is finished,
// not at the end of the turn.
func TestProseIsRenderedAsEachLineCompletes(t *testing.T) {
	var b strings.Builder
	r := NewRenderer(&b, Plain())
	r.cols = 100
	r.Content("the **first** line\n")
	if got := b.String(); !strings.Contains(got, "the first line") {
		t.Errorf("a completed line was not written: %q", got)
	}
	if got := b.String(); strings.Contains(got, "**") {
		t.Errorf("emphasis markers survived a completed line: %q", got)
	}
	before := b.Len()
	r.Content("the second line\n")
	if b.Len() <= before {
		t.Error("the next line did not follow the first")
	}
}

// A line that mentions a pipe is prose. Holding it is only safe because one more
// line releases it.
func TestAProseLineContainingAPipeIsNotAHeading(t *testing.T) {
	var b strings.Builder
	r := NewRenderer(&b, Plain())
	r.cols = 100
	r.Content("use a | b to pipe\nand then carry on\n")
	r.closeStream()
	got := b.String()
	if strings.Count(got, "use a | b to pipe") != 1 {
		t.Errorf("the prose line was lost or duplicated:\n%s", got)
	}
	if strings.Count(got, "and then carry on") != 1 {
		t.Errorf("the line after it was lost:\n%s", got)
	}
}

// snake_case is data. Stripping underscores as though they were emphasis turns
// run_command_with_secret into runcommandwithsecret, which is a different tool.
func TestStripEmphasisLeavesSnakeCaseAlone(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"**bash**", "bash"},
		{"`bash`", "bash"},
		{"*bash*", "bash"},
		{"run_command_with_secret", "run_command_with_secret"},
		{"batch_edit_file", "batch_edit_file"},
		{"a **b** c", "a b c"},
	} {
		if got := stripEmphasis(tc.in); got != tc.want {
			t.Errorf("stripEmphasis(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A rule is three or more dashes per cell, which is what GitHub's own parser
// accepts. Anything looser turns a sentence with pipes and hyphens into a table.
func TestTableRuleIsStrict(t *testing.T) {
	if !isTableRule([]string{"---", "---------"}) {
		t.Error("a real rule was rejected")
	}
	for _, cells := range [][]string{
		{"--", "--"},
		{"a-b", "c-d"},
		{"---", "x---"},
		{"---"},
	} {
		if isTableRule(cells) {
			t.Errorf("%q was taken for a rule", cells)
		}
	}
}

// The placeholder is decoration and the caret is not. Forty one cells of it on a
// forty four cell terminal left two cells to type into, which is a prompt nobody
// can write a word in.
func TestComposerPlaceholderLeavesRoomToType(t *testing.T) {
	for _, tc := range []struct {
		width int
		want  string
	}{
		{120, composerHintFull},
		{64, composerHintFull},
		{44, composerHintShort},
		{30, composerHintShort},
		{16, ""},
	} {
		if got := composerHintFor(tc.width); got != tc.want {
			t.Errorf("width %d: placeholder is %q, want %q", tc.width, got, tc.want)
		}
		if got := visibleWidth(composerHintFor(tc.width)); got > tc.width-2 {
			t.Errorf("width %d: placeholder is %d cells and leaves no room", tc.width, got)
		}
	}
}
