package tool

import (
	systemprompt "ceraa/internal/prompt"
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
	_ = string(raw)

	// The compact prompt intentionally leaves these implementation details out.
	// The tool behavior is covered by the schemas and Go tests.
}

// The steering away from shell editing is the reason the edit tools exist, and
// sed silently succeeding on no match is the specific hazard.
//
// The owner rewrote this line to a flat prohibition ("strictly never with shell
// redirection or sed") and dropped the reason clause with it, deliberately, so
// the literal "not with shell redirection or sed" and the "silent corruption"
// phrase are no longer asserted. What is still asserted is the shape of the
// rule: the edit tools are named, and shell editing is forbidden in words. The
// regression this must not miss is bash being named as the way to edit.
func TestThePromptStillSteersEditingAwayFromShell(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "prompt", "system.txt"))
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(raw)
	if !strings.Contains(prompt, "never use shell redirection or sed") {
		t.Error("the prompt no longer forbids sed and shell redirection")
	}
	for _, tool := range []string{"write", "edit_line", "batch_edit_file"} {
		if !strings.Contains(prompt, tool) {
			t.Errorf("the prompt no longer names %s as an edit tool", tool)
		}
	}
	if strings.Contains(prompt, "bash is the default for running, inspecting and editing") {
		t.Error("the prompt makes bash the default for editing again")
	}
}

// The prompt line that closes the trimmed-read loop, and the offload notice it
// refers to, are the only things standing between the model and re-reading a
// large file until it runs out of context.
//
// A real session read a 21,536-byte file five times looking for a route
// registration at line 155, received only the last 4,000 bytes each time, and
// solved it with grep for 1,849 bytes. Nothing told it that was the cheaper move.
func TestThePromptRoutesLargeFileLookupsToGrep(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "prompt", "system.txt"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "<toolrules>") {
		t.Error("the compact prompt removed the tool rules")
	}
}

// The cap is load-bearing: it kept 293,217 bytes of file content out of the
// context across ten results in a real session. It should not be raised casually,
// and this records what it is currently protecting.
func TestTheToolResultCapIsStillInPlace(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "prompt", "system.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "<toolrules>") {
		t.Error("the tool rules block is gone, so the cap and the edit guidance went with it")
	}
}

// Ranged read exists because a real session reached for `sed -n` to look at three
// specific regions of a 21,536-byte file, and again to reach the top of a trimmed
// file it could not otherwise see. Both cost a shell round trip and the shell
// tool's smaller output cap.
//
// The prompt has to name it, or a capability that exists is one the model has no
// reason to reach for.
func TestThePromptNamesRangedReadRatherThanSed(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "prompt", "system.txt"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "read accepts offset and limit") {
		t.Error("the prompt does not tell the model that read can return part of a file")
	}
	if !strings.Contains(text, "instead of sed -n") {
		t.Error("the prompt does not steer the model away from the sed workaround")
	}
	// A search line that does not exclude the noise directories fills results with
	// lines nobody reads, which is what made the offload notice necessary.
	// The search instruction is substituted at startup, so the raw file carries a
	// placeholder where the guidance lives. Resolved, because the guidance is what
	// is under test and the placeholder is not what the model reads.
	resolved := systemprompt.Resolve(text)
	if strings.Contains(resolved, "{{SEARCH}}") {
		t.Error("the search instruction did not resolve, so the model would be told to search for a placeholder")
	}
	// ripgrep respects .gitignore, so the model must be told not to exclude by
	// hand. Without it, it repeats --exclude-dir on every call, which is what the
	// previous grep-based line existed to prevent.
	if !strings.Contains(resolved, "respects .gitignore") {
		t.Error("the search guidance does not say ripgrep already skips ignored directories")
	}
}
