package menus

import (
	"encoding/json"
	"strings"
	"testing"
)

// A hub entry with no menu case renders as a bare Close button, which is a dead
// end indistinguishable from a broken button. Four subscription providers shipped
// that way: Hermes, xAI, MiniMax and Cline were all listed in the hub and all
// resolved to Close.
func TestEveryHubEntryResolvesToARealMenu(t *testing.T) {
	for _, name := range All {
		keyboard := InlineForChat(name, 0, 1, "")
		raw, err := json.Marshal(keyboard)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		// A Close-only keyboard is the dead end. Anything with a second row, or a
		// different button, is a real menu.
		rows := keyboard["inline_keyboard"].([][]map[string]string)
		dead := true
		for _, row := range rows {
			for _, button := range row {
				if button["callback_data"] != "close" {
					dead = false
				}
			}
		}
		if dead {
			t.Errorf("/%s resolves to a bare Close button: %s", name, raw)
		}
	}
}

// The subscription providers need a real menu with a route into the flow, not
// just a Back row, or the hub row is decorative.
func TestSubscriptionProvidersOfferTheirEntryPoint(t *testing.T) {
	cases := map[string]string{
		"hermes":   "provider:hermes",
		"xai":      "provider:xai",
		"minimax":  "provider:minimax",
		"cline":    "provider:cline",
		"opencode": "provider:opencode",
	}
	labels := map[string]string{
		"hermes": "Hermes", "xai": "xAI", "minimax": "MiniMax",
		"cline": "Cline", "opencode": "OpenCode Zen",
	}
	for name, entry := range cases {
		raw, _ := json.Marshal(InlineForChat(name, 0, 1, ""))
		// The label names the provider, so a menu reached by tapping Cline says
		// Cline rather than something the user has to decode.
		if !strings.Contains(string(raw), labels[name]) {
			t.Errorf("/%s does not name %s on its buttons: %s", name, labels[name], raw)
		}
		if !strings.Contains(string(raw), entry) {
			t.Errorf("/%s has no route to %s: %s", name, entry, raw)
		}
		if !strings.Contains(string(raw), "menu:hub") {
			t.Errorf("/%s offers no way back: %s", name, raw)
		}
	}
}

// The hub label and the menu must agree about state, or tapping through changes
// the answer.
func TestSubscriptionMenuStates(t *testing.T) {
	if !strings.Contains(mustJSON(subscriptionMenu("Test", "🔑", false, false, "provider:x")), "Sign in") {
		t.Error("an unsigned provider does not offer sign-in")
	}
	if !strings.Contains(mustJSON(subscriptionMenu("Test", "🔑", true, false, "provider:x")), "Switch to") {
		t.Error("a signed-in but inactive provider does not offer a switch")
	}
	if !strings.Contains(mustJSON(subscriptionMenu("Test", "🔑", true, true, "provider:x")), "is active") {
		t.Error("an active provider does not say so")
	}
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// Every "menu:" callback_data any menu emits has to resolve to a real menu.
//
// This walks what the menus actually emit rather than what All lists, and that
// difference is the whole defect. All does not contain "hub" - correctly, since
// the hub is the screen the hub links to rather than an entry it lists - so the
// guard that walks All could never see the name. Four menus emitted "menu:hub" as
// their Back row (ProviderMenu, ModelMenu, subscriptionMenu, CodexMenu) and with
// no case for it that data fell through to the default closeMenu(). Back from any
// provider or model screen answered with a message reading "📋 hub" carrying
// nothing but a Close button.
//
// The lesson is the deny-list one from the secrets work: a name that resolves
// through a switch is a list, and anything outside it fails silently. Enumerating
// the emitted names makes the negative space the thing under test.
func TestEveryEmittedMenuCallbackResolvesToARealMenu(t *testing.T) {
	build := map[string]map[string]any{"hub": Hub()}
	for _, name := range All {
		build[name] = InlineForChat(name, 0, 1, "")
	}

	seen := map[string]string{}
	for source, keyboard := range build {
		rows, ok := keyboard["inline_keyboard"].([][]map[string]string)
		if !ok {
			continue
		}
		for _, row := range rows {
			for _, button := range row {
				data := button["callback_data"]
				if !strings.HasPrefix(data, "menu:") {
					continue
				}
				seen[strings.TrimPrefix(data, "menu:")] = source
			}
		}
	}

	if len(seen) == 0 {
		t.Fatal("no menu callbacks emitted at all, so this guard is not looking at anything")
	}
	for name, source := range seen {
		resolved := InlineForChat(name, 0, 1, "")
		if isBareClose(resolved) {
			t.Errorf("%s offers Back as menu:%s, which resolves to a bare Close button", source, name)
		}
	}
}

// isBareClose reports whether a menu carries nothing but the Close button, which
// is what a name with no resolver case falls back to.
func isBareClose(keyboard map[string]any) bool {
	rows, ok := keyboard["inline_keyboard"].([][]map[string]string)
	if !ok {
		return true
	}
	for _, row := range rows {
		for _, button := range row {
			if button["callback_data"] != "close" {
				return false
			}
		}
	}
	return true
}

// The hub has to actually render every entry it is supposed to show, with a
// label a person can read.
//
// Dispatch and rendering are separate defects and only the first was covered.
// OpenCode Zen resolved to a real menu the whole time and still never appeared.
// Walking All alone cannot see that, because the entry was missing from All
// itself: an absent provider is checked by nothing, so the loop below has to
// start from a list written by hand rather than from the thing that was wrong.
// A row that exists in the router but not on screen is invisible to any test
// derived from the router, which is what the screenshot showed.
func TestEveryHubEntryIsRenderedWithALabel(t *testing.T) {
	keyboard := Hub()
	rows, ok := keyboard["inline_keyboard"].([][]map[string]string)
	if !ok {
		t.Fatalf("Hub() returned %T for inline_keyboard", keyboard["inline_keyboard"])
	}
	rendered := map[string]string{}
	for _, row := range rows {
		for _, button := range row {
			data := button["callback_data"]
			if !strings.HasPrefix(data, "menu:") {
				continue
			}
			rendered[strings.TrimPrefix(data, "menu:")] = button["text"]
		}
	}
	// Written independently of All, which is the list that was missing an entry.
	mustRender := []string{
		"codex", "hermes", "xai", "minimax", "cline", "opencode",
		"status", "provider", "model", "context",
	}
	for _, name := range mustRender {
		label, ok := rendered[name]
		if !ok {
			t.Errorf("the hub renders no row for %q; it is listed but invisible", name)
			continue
		}
		if strings.TrimSpace(label) == "" {
			t.Errorf("the hub renders %q with an empty label", name)
		}
	}
	// Everything All does list has to carry a label too, so a newly added entry
	// cannot arrive as a blank button.
	for _, name := range All {
		if hubHidden[name] {
			continue
		}
		if label, ok := rendered[name]; !ok {
			t.Errorf("%q is in All but the hub renders no row for it", name)
		} else if strings.TrimSpace(label) == "" {
			t.Errorf("the hub renders %q with an empty label", name)
		}
	}
}
