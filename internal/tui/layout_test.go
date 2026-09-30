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
	if strings.Contains(lines[1], unset) && strings.Contains(lines[1], "model  "+unset) == false {
		t.Errorf("the unset marker is not attached to the model row:\n%s", joined)
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
