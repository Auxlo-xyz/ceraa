package menus

import (
	"encoding/json"
	"strings"
	"testing"
)

// Codex status used to be reachable only by typing /codex-status, so the whole
// auth flow was unusable from the menus.
func TestHubNamesCodex(t *testing.T) {
	raw, _ := json.Marshal(Hub())
	if !strings.Contains(string(raw), "codex") {
		t.Errorf("hub does not offer a Codex entry: %s", raw)
	}
	if !strings.Contains(string(raw), "Codex") {
		t.Errorf("hub has no human-readable Codex label: %s", raw)
	}
}

// Whichever provider is active, the Codex screen must offer something to press.
func TestCodexMenuAlwaysOffersAnAction(t *testing.T) {
	for _, active := range []string{"codex", "custom", ""} {
		raw, _ := json.Marshal(CodexMenu(false, active))
		if !strings.Contains(string(raw), "callback_data") {
			t.Errorf("active=%q: codex menu has no action: %s", active, raw)
		}
	}
}

// The signed-in-but-inactive state is the one that needs a switch button, and it
// is easy to leave out because it is not the state you are in while testing.
func TestCodexMenuOffersASwitchWhenSignedInElsewhere(t *testing.T) {
	raw, _ := json.Marshal(CodexMenu(true, "custom"))
	if !strings.Contains(string(raw), "codex:activate") {
		t.Errorf("signed in on another provider but no switch button: %s", raw)
	}
}

func TestCodexMenuOffersSignInWhenNotAuthenticated(t *testing.T) {
	raw, _ := json.Marshal(CodexMenu(false, ""))
	if !strings.Contains(string(raw), "codex:login") {
		t.Errorf("not signed in but no sign-in button: %s", raw)
	}
}

func TestCodexHubLabelAlwaysNamesCodex(t *testing.T) {
	if label := codexHubLabel(); !strings.Contains(label, "Codex") {
		t.Errorf("hub label %q does not name Codex", label)
	}
}
