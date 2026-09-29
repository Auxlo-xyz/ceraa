package telegram

import (
	"path/filepath"
	"testing"

	"ceraa/internal/config"
	"ceraa/internal/telegram/menus"
)

// Switching a provider has to be visible to the next menu render, not only to the
// next run.
//
// This failed live: the switch reported "xAI/Grok provider activated with model
// grok-4.7", and the Hub and /model rendered a minute later still showed
// "MiniMax · active" and "Provider: minimax". Restarting fixed it, which is what
// made it look like the write had not landed at all.
func TestProviderSwitchIsVisibleToTheNextMenu(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	previous := providers
	providers = newProviderState(config.NewStoreAt(filepath.Join(dir, "config.json")))
	defer func() { providers = previous }()

	if err := defaultState.setProvider("minimax"); err != nil {
		t.Fatalf("seed minimax: %v", err)
	}
	seedSwitchProfile(t, "minimax", "MiniMax-M3", "minimax")
	seedSwitchProfile(t, "xai", "grok-4.7", "xai")

	// Before the switch, MiniMax should read as active.
	if !menuMentionsActive("minimax") {
		t.Fatalf("precondition: MiniMax should be active before the switch")
	}

	// The switch.
	if err := defaultState.setProvider("xai"); err != nil {
		t.Fatalf("switch to xai: %v", err)
	}

	// The file is the single source of truth, so confirm it first: if this fails,
	// the write never landed and the menu is a red herring.
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.ActiveProvider != "xai" {
		t.Fatalf("config.json says %q after the switch, so the write did not land", cfg.ActiveProvider)
	}

	sel, err := config.LoadSelection()
	if err != nil {
		t.Fatalf("load selection: %v", err)
	}
	if sel.ActiveProvider != "xai" {
		t.Errorf("LoadSelection says %q, want xai; this is what the menus read", sel.ActiveProvider)
	}
	if menuMentionsActive("minimax") {
		t.Error("the provider menu still marks MiniMax active after switching to xAI")
	}
}

// menuMentionsActive reads the provider menu the Hub renders, so the test exercises
// the same code path the screenshot came from rather than a reimplementation.
func menuMentionsActive(providerID string) bool {
	_, active := menus.ProviderEntries()
	return active == providerID
}

func seedSwitchProfile(t *testing.T, name, model, subscription string) {
	t.Helper()
	if err := defaultState.setProviderProfile(name, config.ProviderProfile{
		Model: model, Subscription: subscription,
	}); err != nil {
		t.Fatalf("seed profile %s: %v", name, err)
	}
}
