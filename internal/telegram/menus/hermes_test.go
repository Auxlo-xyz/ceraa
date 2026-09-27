package menus

import (
	"encoding/json"
	"strings"
	"testing"
)

// The gap this closes: Hermes existed only in onboarding, so a user who had
// already signed in had no way to reach it from the Provider menu. Codex got a
// dedicated row for exactly that reason, and the comment there said so.
func TestProviderMenuOffersHermesLikeCodex(t *testing.T) {
	raw, _ := json.Marshal(ProviderMenu())
	for _, want := range []string{"provider:codex", "provider:hermes"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the provider menu has no %q button: %s", want, raw)
		}
	}
}

// The label has to name Hermes and say something about state, or it is a dead
// end with no hint that the provider is even configured.
func TestProviderMenuHermesLabelIsHumanReadable(t *testing.T) {
	raw, _ := json.Marshal(ProviderMenu())
	if !strings.Contains(string(raw), "Hermes") {
		t.Errorf("the hermes button does not name Hermes: %s", raw)
	}
	if !strings.Contains(string(raw), "sign in") && !strings.Contains(string(raw), "switch to it") && !strings.Contains(string(raw), "\u2705") {
		t.Errorf("the hermes button does not report its state: %s", raw)
	}
}

// The hub answers "is Hermes signed in" without opening the provider menu, the
// same way the Codex entry does.
func TestHubNamesHermes(t *testing.T) {
	raw, _ := json.Marshal(Hub())
	if !strings.Contains(string(raw), "menu:hermes") {
		t.Errorf("the hub has no hermes row: %s", raw)
	}
	if !strings.Contains(string(raw), "Hermes") {
		t.Errorf("the hub has no human-readable Hermes label: %s", raw)
	}
}

// xAI gets a dedicated row for the same reason as Codex and Hermes: a
// subscription profile cannot appear as a profile row until it exists, so without
// this a signed-in user has no way to reach it from the provider menu.
func TestProviderMenuOffersXaiLikeCodex(t *testing.T) {
	raw, _ := json.Marshal(ProviderMenu())
	if !strings.Contains(string(raw), "provider:xai") {
		t.Errorf("the provider menu has no xai button: %s", raw)
	}
	if !strings.Contains(string(raw), "xAI") {
		t.Errorf("the xai button does not name xAI: %s", raw)
	}
}

func TestHubNamesXai(t *testing.T) {
	raw, _ := json.Marshal(Hub())
	if !strings.Contains(string(raw), "menu:xai") {
		t.Errorf("the hub has no xai row: %s", raw)
	}
	if !strings.Contains(string(raw), "xAI") {
		t.Errorf("the hub has no human-readable xAI label: %s", raw)
	}
}

func TestProviderMenuOffersMinimaxLikeCodex(t *testing.T) {
	raw, _ := json.Marshal(ProviderMenu())
	if !strings.Contains(string(raw), "provider:minimax") {
		t.Errorf("the provider menu has no minimax button: %s", raw)
	}
	if !strings.Contains(string(raw), "MiniMax") {
		t.Errorf("the minimax button does not name MiniMax: %s", raw)
	}
}

func TestHubNamesMinimax(t *testing.T) {
	raw, _ := json.Marshal(Hub())
	if !strings.Contains(string(raw), "menu:minimax") {
		t.Errorf("the hub has no minimax row: %s", raw)
	}
	if !strings.Contains(string(raw), "MiniMax") {
		t.Errorf("the hub has no human-readable MiniMax label: %s", raw)
	}
}

func TestProviderMenuOffersClineLikeCodex(t *testing.T) {
	raw, _ := json.Marshal(ProviderMenu())
	if !strings.Contains(string(raw), "provider:cline") {
		t.Errorf("the provider menu has no cline button: %s", raw)
	}
	if !strings.Contains(string(raw), "Cline") {
		t.Errorf("the cline button does not name Cline: %s", raw)
	}
}

func TestHubNamesCline(t *testing.T) {
	raw, _ := json.Marshal(Hub())
	if !strings.Contains(string(raw), "menu:cline") {
		t.Errorf("the hub has no cline row: %s", raw)
	}
	if !strings.Contains(string(raw), "Cline") {
		t.Errorf("the hub has no human-readable Cline label: %s", raw)
	}
}
