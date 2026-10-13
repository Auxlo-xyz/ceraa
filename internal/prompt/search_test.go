package prompt

import (
	"strings"
	"testing"

	sourceprompt "ceraa/prompt"
)

// ripgrep is the search tool, unconditionally. The prompt says so, so ripgrep
// either exists or the deployment is broken and says so.
func TestTheSearchLineNamesRipgrepAndNothingElse(t *testing.T) {
	if !strings.Contains(searchLine, "rg -n") || !strings.Contains(searchLine, "rg --files") {
		t.Errorf("the search line does not give the invocations: %q", searchLine)
	}
	// The flags are the reason to prefer it over a recursive grep, so they have to
	// be in the line or the model will invent its own invocation and lose them.
	if !strings.Contains(searchLine, "-g") {
		t.Error("the line does not mention -g for type filtering")
	}
	if !strings.Contains(searchLine, ".gitignore") {
		t.Error("the line does not say ripgrep already respects .gitignore, so the model will exclude by hand")
	}
	// There is no fallback. A name in the prompt is a dependency, and naming a
	// second tool invites the model to pick the wrong one.
	if strings.Contains(searchLine, "grep -rn") {
		t.Errorf("the search line still names grep: %q", searchLine)
	}
}

// The prompt must not vary by deployment. It is in the cached prefix for every
// turn, so anything environment-dependent there costs on every request.
func TestTheSearchLineIsStatic(t *testing.T) {
	first := resolveSearchTool("{{SEARCH}}", "test")
	second := resolveSearchTool("{{SEARCH}}", "test")
	if first != second {
		t.Error("the same input resolved to two different prompts")
	}
	if first != searchLine {
		t.Errorf("resolved to %q, want exactly the static line", first)
	}
}

// A prompt with no placeholder is passed through untouched, which is what keeps
// CERAA_SYSTEM_PROMPT working.
func TestAPromptWithNoPlaceholderIsUntouched(t *testing.T) {
	text := "a prompt that says nothing about search"
	if got := resolveSearchTool(text, "test"); got != text {
		t.Errorf("got %q, want it unchanged", got)
	}
}

// The shipped file must resolve, or the model receives a literal placeholder and
// goes looking for the string "{{SEARCH}}".
func TestTheShippedPromptResolvesToNoPlaceholder(t *testing.T) {
	resolved := Resolve(string(sourceprompt.System))
	if strings.Contains(resolved, "{{") {
		t.Error("the shipped prompt still contains a placeholder after substitution")
	}
	if !strings.Contains(resolved, "rg -n") {
		t.Error("the shipped prompt does not end up naming ripgrep")
	}
	// grep must be gone from the search guidance, not merely absent from the line.
	for _, line := range strings.Split(resolved, "\n") {
		if strings.Contains(line, "Search the tree") && strings.Contains(line, "grep -rn") {
			t.Errorf("the search guidance still tells the model to use grep: %q", line)
		}
	}
}
