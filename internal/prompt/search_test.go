package prompt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The search line has to match the deployment, and a bot serving several users
// runs in images that are not the same image. Naming ripgrep where it is absent
// teaches the model to reach for a binary that will fail.
func TestTheSearchLineNamesTheToolThisDeploymentHas(t *testing.T) {
	template := "before {{SEARCH}} after"

	_, rgErr := exec.LookPath("rg")
	resolved := resolveSearchTool(template, "test")

	if strings.Contains(resolved, searchPlaceholder) {
		t.Fatal("the placeholder survived substitution and would reach the model as literal text")
	}
	if rgErr == nil {
		if !strings.Contains(resolved, "rg -n") {
			t.Errorf("ripgrep is installed but the line does not name it: %q", resolved)
		}
		// The flags are the point. A bare "use rg" leaves the model inventing its
		// own invocation and losing the gitignore behaviour.
		if !strings.Contains(resolved, "rg --files") || !strings.Contains(resolved, "gitignore") {
			t.Errorf("the line names ripgrep but not how to use it: %q", resolved)
		}
	} else if !strings.Contains(resolved, "grep -rn") {
		t.Errorf("ripgrep is absent, so the line must name grep: %q", resolved)
	}
}

// The fallback has to carry the exclusions by hand, because grep has no default
// and the noise directories are what filled a result once already.
func TestTheGrepFallbackExcludesTheNoiseDirectories(t *testing.T) {
	for _, want := range []string{"node_modules", ".git", "grep -rn"} {
		if !strings.Contains(searchLineGrep, want) {
			t.Errorf("the grep fallback does not mention %q: %q", want, searchLineGrep)
		}
	}
}

// A prompt with no placeholder is passed through untouched, which is what makes
// CERAA_SYSTEM_PROMPT still work.
func TestAPromptWithNoPlaceholderIsUntouched(t *testing.T) {
	text := "a prompt that says nothing about search"
	if got := resolveSearchTool(text, "test"); got != text {
		t.Errorf("got %q, want it unchanged", got)
	}
}

// The real file must not ship a placeholder that failed to resolve.
func TestTheShippedPromptHasNoUnresolvedPlaceholder(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", SystemPromptPath))
	if err != nil {
		t.Skipf("prompt not readable from this package: %v", err)
	}
	resolved := resolveSearchTool(string(raw), SystemPromptPath)
	if strings.Contains(resolved, "{{") {
		t.Error("the shipped prompt still contains a placeholder after substitution")
	}
	if !strings.Contains(resolved, "There is no separate grep or ls tool") {
		t.Error("the surrounding search guidance was lost with the substitution")
	}
}
