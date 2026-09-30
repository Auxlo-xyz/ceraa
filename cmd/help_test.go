package cmd

import (
	"strings"
	"testing"
)

// linesOver reports the rows of s wider than width, in cells. Help output is
// plain ASCII, so bytes and cells agree here.
func linesOver(s string, width int) []string {
	var bad []string
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if len(l) > width {
			bad = append(bad, l)
		}
	}
	return bad
}

// The phone screen that started this: cobra's default template prints every
// description verbatim, and the terminal finished the long ones mid-word.
func TestRootHelpFitsANarrowTerminal(t *testing.T) {
	registerSubcommands()
	got := renderHelp(root, 40)
	if bad := linesOver(got, 40); len(bad) > 0 {
		t.Errorf("root help overflows 40 columns:\n%s", strings.Join(bad, "\n"))
	}
}

// Wrapping must join back into the untouched description. If a word was cut,
// the join does not match.
func TestRootHelpWrapsWordsWhole(t *testing.T) {
	registerSubcommands()
	got := renderHelp(root, 40)
	cmds := availableHelpCommands(root)
	pad := 0
	for _, c := range cmds {
		if n := len(c.Name()); n > pad {
			pad = n
		}
	}
	pad += 2
	col := 2 + pad
	for _, c := range cmds {
		if strings.TrimSpace(c.Short) == "" {
			continue
		}
		rows := helpCommandRow(c.Name(), c.Short, pad, 40)
		joined := strings.TrimSpace(rows[0][col:])
		for _, r := range rows[1:] {
			joined += " " + strings.TrimSpace(r)
		}
		if joined != c.Short {
			t.Errorf("%s: wrapped description joins back wrong:\n got %q\nwant %q", c.Name(), joined, c.Short)
		}
		if !strings.Contains(got, c.Name()) {
			t.Errorf("root help lost the %s row", c.Name())
		}
	}
}

// Continuation lines hang under the description column, not the name column,
// or the table stops reading as a table after the first wrap.
func TestRootHelpHangsContinuationsUnderTheDescription(t *testing.T) {
	rows := helpCommandRow("completion", "Generate the autocompletion script for the specified shell", 13, 40)
	if len(rows) < 2 {
		t.Fatalf("expected the description to wrap, got %q", rows)
	}
	for _, r := range rows[1:] {
		if !strings.HasPrefix(r, strings.Repeat(" ", 15)) {
			t.Errorf("continuation not hung at column 15: %q", r)
		}
	}
}

// When the names eat the row, squeezing the description beside them wraps every
// other word. Underneath on its own lines reads better.
func TestHelpRowDropsLongDescriptionsUnderneath(t *testing.T) {
	rows := helpCommandRow("config-init", "Write a sample config file, refusing to overwrite an existing one", 13, 20)
	if len(rows) < 2 || rows[0] != "  config-init" {
		t.Fatalf("expected the name alone on the first row, got %q", rows)
	}
	for _, r := range rows[1:] {
		if !strings.HasPrefix(r, "    ") {
			t.Errorf("description row not indented underneath: %q", r)
		}
		if len(r) > 20 {
			t.Errorf("description row overflows: %q", r)
		}
	}
}

func TestLeafHelpFitsANarrowTerminal(t *testing.T) {
	registerSubcommands()
	for _, c := range root.Commands() {
		got := renderHelp(c, 40)
		if bad := linesOver(got, 40); len(bad) > 0 {
			t.Errorf("%s help overflows 40 columns:\n%s", c.Name(), strings.Join(bad, "\n"))
		}
	}
}

func TestHelpRendererIsWiredToEveryCommand(t *testing.T) {
	registerSubcommands()
	if root.HelpFunc() == nil {
		t.Error("root has no help function")
	}
	for _, c := range root.Commands() {
		if c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		if c.HelpFunc() == nil {
			t.Errorf("%s has no help function", c.Name())
		}
	}
}
