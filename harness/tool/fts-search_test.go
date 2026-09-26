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
	if !strings.Contains(string(prompt), "rg via bash") {
		t.Error("the prompt no longer points exact-regex search at rg via bash")
	}
}
