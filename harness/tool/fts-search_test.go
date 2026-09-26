package tool

import (
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
	if !strings.Contains(description, "bash") {
		t.Errorf("description does not say where exact regex search now goes: %q", description)
	}
}
