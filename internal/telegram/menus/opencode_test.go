package menus

import (
	"encoding/json"
	"strings"
	"testing"
)

// The catalog question belongs under Model, which is the only surface that can
// show a list beside the answer. Onboarding asks for a key, so this is where a
// user who does not know an id goes to find one. Both spellings of the entry
// reach it: the menu name and the callback data the menu emits.
func TestOpenCodeModelCatalogIsReachableFromItsMenu(t *testing.T) {
	for _, name := range []string{"opencode", "provider:opencode"} {
		keyboard := InlineForChat(name+":models", 0, 1, "")
		raw, err := json.Marshal(keyboard)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(string(raw), "provider:opencode:model:") {
			t.Errorf("%s:models offered no model to pick: %s", name, raw)
		}
	}
}

// A pasted key is not a sign-in. Offering "Sign in" promises a login screen that
// does not exist for a provider whose whole flow is paste a key.
func TestOpenCodeMenuWordingIsAboutAKey(t *testing.T) {
	keyboard := subscriptionAuthMenu("OpenCode Zen", false, false, "provider:opencode", true)
	raw, _ := json.Marshal(keyboard)
	if strings.Contains(string(raw), "Sign in") {
		t.Errorf("the unsigned OpenCode screen still says sign in: %s", raw)
	}
	if !strings.Contains(string(raw), "Add OpenCode Zen key") {
		t.Errorf("the unsigned OpenCode screen offers no key button: %s", raw)
	}
	active := subscriptionAuthMenu("OpenCode Zen", true, true, "provider:opencode", true)
	raw, _ = json.Marshal(active)
	if !strings.Contains(string(raw), "Replace key") {
		t.Errorf("the active OpenCode screen offers no way to replace the key: %s", raw)
	}
}

// The OAuth providers keep their wording, so the key-based branch is not
// applied to a login flow either.
func TestLoginProvidersKeepSignInWording(t *testing.T) {
	raw, _ := json.Marshal(subscriptionAuthMenu("Hermes", false, false, "provider:hermes", false))
	if !strings.Contains(string(raw), "Sign in to Hermes") {
		t.Errorf("a login provider lost its sign-in button: %s", raw)
	}
}
