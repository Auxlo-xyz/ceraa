package tui

import (
	"strings"
	"testing"
)

// An empty cell reads as a rendering fault rather than as an absent setting: the
// label is present, so the reader looks for a value and finds nothing. A fresh
// install with no provider configured printed "model" and then trailing
// whitespace, which looked like a bug rather than an unconfigured install.
func TestBlockRendersAnEmptyValueAsUnset(t *testing.T) {
	b := &block{title: "status"}
	b.add("model", "")
	b.add("provider", "default")
	lines := b.render(Plain(), "  ", 80)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, unset) {
		t.Errorf("empty value rendered nothing, so the row looks broken:\n%s", joined)
	}
	// The marker belongs on the model's own line. Asserted on both halves of the
	// line rather than on the gap between them, so changing the gap to align a
	// value column cannot fail this.
	if !strings.Contains(lines[1], "model") || !strings.Contains(lines[1], unset) {
		t.Errorf("the unset marker is not attached to the model row:\n%s", joined)
	}
}

// Labels share one edge and values share one column, both measured off the first
// row. The startup block, /status and the banner all render through here, so a
// ragged left edge here is a ragged left edge on screen.
func TestBlockAlignsLabelsAndValues(t *testing.T) {
	b := &block{title: "status"}
	b.add("model", "gpt-5.6-luna")
	b.add("provider", "openai")
	b.add("cwd", "/workspace")
	lines := b.render(Plain(), "  ", 80)
	if len(lines) != 4 {
		t.Fatalf("expected a title and three rows, got %d: %q", len(lines), lines)
	}
	rows := lines[1:]
	labelCol := strings.Index(rows[0], "model")
	valueCol := strings.Index(rows[0], "gpt-5.6-luna")
	for _, row := range rows[1:] {
		fields := strings.Fields(row)
		if len(fields) != 2 {
			t.Fatalf("row is not a label and a value: %q", row)
		}
		if got := strings.Index(row, fields[0]); got != labelCol {
			t.Errorf("label starts at column %d, want %d: %q", got, labelCol, row)
		}
		if got := strings.Index(row, fields[1]); got != valueCol {
			t.Errorf("value starts at column %d, want %d: %q", got, valueCol, row)
		}
	}
}

// Trailing whitespace is invisible in a terminal but survives into logs, diffs and
// copy-paste, where it makes a line differ from an otherwise identical one.
func TestBlockEmitsNoTrailingWhitespace(t *testing.T) {
	b := &block{}
	b.add("model", "")
	b.add("session", "cli-1")
	for i, line := range b.render(Plain(), "  ", 80) {
		if strings.TrimRight(line, " ") != line {
			t.Errorf("line %d has trailing whitespace: %q", i, line)
		}
	}
}

// A title and a row must not be confusable. The title is bold and sits on its own
// line; if that ever regresses the heading reads as a row missing its value.
func TestBlockTitleIsDistinctFromItsRows(t *testing.T) {
	b := &block{title: "status"}
	b.add("model", "gpt")
	lines := b.render(Plain(), "  ", 80)
	if len(lines) != 2 {
		t.Fatalf("expected a title and one row, got %d lines: %q", len(lines), lines)
	}
	if strings.TrimSpace(lines[0]) != "status" {
		t.Errorf("first line is not the title alone: %q", lines[0])
	}
	if strings.Contains(lines[0], "model") {
		t.Errorf("the title line carries row content: %q", lines[0])
	}
}

// A title sits on the same margin as its rows. It is applied the block's indent
// here, so a title string that carried its own put the heading a column right of
// everything under it, and nothing caught it because the text was still present.
func TestBlockTitleSharesTheRowMargin(t *testing.T) {
	b := &block{title: "status"}
	b.add("model", "gpt")
	lines := b.render(Plain(), "  ", 80)
	edge := len(lines[1]) - len(strings.TrimLeft(lines[1], " "))
	if got := len(lines[0]) - len(strings.TrimLeft(lines[0], " ")); got != edge {
		t.Errorf("title starts at column %d, rows at %d: %q", got, edge, lines[0])
	}
	// An indent in the title itself is not honoured twice.
	b2 := &block{title: "  status"}
	b2.add("model", "gpt")
	if got := b2.render(Plain(), "  ", 80)[0]; got != "  status" {
		t.Errorf("title indent was applied twice: %q", got)
	}
}
