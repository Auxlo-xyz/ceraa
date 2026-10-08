package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readPrompt(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "prompt", "system.txt"))
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	return string(raw)
}

// Guidance now lives in the system prompt, so every tool has to be named there.
// If a tool is added and the prompt is not updated, the model gets a schema with
// no usage context, which is the state the prompt section exists to prevent.
func TestPromptNamesEveryRegisteredTool(t *testing.T) {
	prompt := readPrompt(t)
	var missing []string
	for _, tool := range DefaultRegistryForUser("x", nil).All() {
		if !strings.Contains(prompt, tool.Name()) {
			missing = append(missing, tool.Name())
		}
	}
	// send_media registers only in the Telegram path, so the prompt hedges rather
	// than naming it flat.
	if len(missing) == 1 && missing[0] == "send_media" {
		return
	}
	if len(missing) > 0 {
		t.Errorf("the prompt does not mention: %v", missing)
	}
}

// A description states what a tool is; the prompt states when to use it. Putting
// the guidance back in the description is what this split exists to prevent, so
// the common offenders are pinned here.
func TestDescriptionsCarryNoUsageGuidance(t *testing.T) {
	// Deliberately broad: an earlier version of this list held "Use this to" and
	// missed "Use this for", so a mutation that put guidance back passed. The
	// guard has to match the shape of the mistake, not one phrasing of it.
	phrases := []string{
		"use this", "use when", "use it to", "prefer this", "prefer it",
		"rather than", "ask free-form when", "instead of asking",
		"e.g. session persistence", "beyond knowledge cutoff",
	}
	for _, tool := range DefaultRegistryForUser("x", nil).All() {
		description := tool.Description()
		lower := strings.ToLower(description)
		for _, phrase := range phrases {
			if strings.Contains(lower, phrase) {
				t.Errorf("%s description carries usage guidance (%q); that belongs in the prompt's toolrules section",
					tool.Name(), phrase)
			}
		}
	}
}

// The behavioural facts a schema cannot express must survive the trim.
func TestDescriptionsKeepBehaviourFacts(t *testing.T) {
	reg := DefaultRegistryForUser("x", nil)
	for _, tool := range reg.All() {
		switch tool.Name() {
		case "browser":
			// Which arguments belong to which action is the whole defence against
			// the 0/14 misuse, and it lives only here.
			if !strings.Contains(tool.Description(), "rejected") {
				t.Error("browser description lost the rule that a mismatched argument is rejected")
			}
		case "question":
			if !strings.Contains(tool.Description(), "buttons") {
				t.Error("question description lost the fact that options become buttons")
			}
		case "read":
			if !strings.Contains(tool.Description(), "directory") {
				t.Error("read description lost the fact that a directory path lists it")
			}
		}
	}
}
