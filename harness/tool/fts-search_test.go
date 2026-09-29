package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The description used to send the model to a grep tool that no longer exists.
// Exact-regex search goes through rg via bash now, which is what the recorded
// sessions actually did: 64 shell searches against 5 grep calls.
func TestFTSSearchPointsAtBashNotARemovedTool(t *testing.T) {
	description := (&FTSSearchTool{}).Description()
	if strings.Contains(description, "use grep") {
		t.Errorf("description still points at the removed grep tool: %q", description)
	}
	// The replacement guidance is prompt-side now, so assert it survived there.
	prompt, err := os.ReadFile(filepath.Join("..", "..", "prompt", "system.txt"))
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	text := string(prompt)
	// What this guards is the routing, not one phrasing of it: exact-regex search
	// has to go through bash, because there is no grep tool to send the model to.
	//
	// The wording changed when ripgrep was dropped from the prompt, because rg is
	// not installed on this host and naming a binary that is not there teaches the
	// model to reach for a tool that will fail. The assertion follows the intent.
	if !strings.Contains(text, "fts_search") {
		t.Error("the prompt no longer mentions fts_search at all")
	}
	if !strings.Contains(text, "bash") || !strings.Contains(text, "grep -rn") {
		t.Error("the prompt no longer points exact-regex search at bash")
	}
	// Naming a binary that is not installed is the mistake worth preventing, since
	// it reads as authoritative and the call fails.
	if strings.Contains(text, "rg ") {
		t.Error("the prompt still names rg, which is not installed on this host")
	}
}
