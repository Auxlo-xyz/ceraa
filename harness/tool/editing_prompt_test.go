package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The prompt and the edit tools drifted apart once: edit_line had grown an
// occurrence argument and named its match lines, while the prompt still said
// "unique" and "matches twice". The model reads the prompt, so a stale line
// there is a real defect, not a documentation nit.
//
// Asserted as intent rather than as one phrasing, the way the fts_search guard
// does: what matters is that occurrence is offered and that the failure modes
// are described, not the exact wording.
func TestThePromptDescribesTheEditToolsAsTheyBehave(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "prompt", "system.txt"))
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(raw)

	// The old wording is the specific claim that went stale: with occurrence,
	// old_text no longer has to be unique.
	if strings.Contains(prompt, "one exact, unique occurrence") {
		t.Error("the prompt still says old_text must be unique, but occurrence lets the caller choose a repeat")
	}
	if !strings.Contains(prompt, "occurrence") {
		t.Error("the prompt never mentions occurrence, so the model has no reason to pass it")
	}
	// The two failure modes the model can now act on directly.
	if !strings.Contains(prompt, "whitespace") {
		t.Error("the prompt does not mention the whitespace near-match, the most common way an edit fails to match")
	}
	if !strings.Contains(prompt, "line") {
		t.Error("the prompt does not mention that a failure names the line")
	}
	// All-or-nothing plus reporting everything, which is what makes a batch worth
	// one call rather than several.
	if !strings.Contains(prompt, "all or nothing") {
		t.Error("the prompt no longer states that a batch is all or nothing")
	}
	if !strings.Contains(prompt, "every edit that did not match") {
		t.Error("the prompt does not say a batch reports all its failures, not just the first")
	}
}

// The steering away from shell editing is the reason the edit tools exist, and
// sed silently succeeding on no match is the specific hazard.
func TestThePromptStillSteersEditingAwayFromShell(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "prompt", "system.txt"))
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(raw)
	if !strings.Contains(prompt, "not with shell redirection or sed") {
		t.Error("the prompt no longer steers editing away from sed and shell redirection")
	}
	if !strings.Contains(prompt, "silent corruption") {
		t.Error("the prompt no longer says why: a shell edit succeeds whether or not the file matched")
	}
}
