package telegram

import (
	"path/filepath"
	"testing"

	"ceraa/internal/config"
)

func newTestState(t *testing.T) *bridgeState {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	state := loadBridgeStateFromFile(path)
	if state.loadErr != nil {
		t.Fatalf("load state: %v", state.loadErr)
	}
	return state
}

func TestRemoveProviderProfileDeletesOnlyTheTarget(t *testing.T) {
	state := newTestState(t)
	for _, id := range []string{"codex", "backup"} {
		if err := state.setProviderProfile(id, config.ProviderProfile{Model: "m-" + id}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if _, err := state.removeProviderProfile("codex"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	profiles := state.providerProfiles()
	if _, still := profiles["codex"]; still {
		t.Error("codex survived removal")
	}
	if _, kept := profiles["backup"]; !kept {
		t.Error("removal took an unrelated provider with it")
	}
}

// Deleting the active provider must not leave the bot pointing at a profile that
// no longer exists, which would fail every later request.
func TestRemoveActiveProviderFallsBackToDefault(t *testing.T) {
	state := newTestState(t)
	if err := state.setProviderProfile("codex", config.ProviderProfile{Model: "gpt-5.6-luna"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := state.setProvider("codex"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if _, err := state.removeProviderProfile("codex"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := state.provider(); got != "default" {
		t.Errorf("active provider = %q, want default", got)
	}
}

func TestRemoveActiveProviderKeepsActiveWhenRemovingAnother(t *testing.T) {
	state := newTestState(t)
	for _, id := range []string{"codex", "other"} {
		if err := state.setProviderProfile(id, config.ProviderProfile{Model: "m"}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if err := state.setProvider("codex"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if _, err := state.removeProviderProfile("other"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := state.provider(); got != "codex" {
		t.Errorf("active provider = %q, want it unchanged", got)
	}
}

func TestRemoveProviderRefusesDefault(t *testing.T) {
	state := newTestState(t)
	if _, err := state.removeProviderProfile("default"); err == nil {
		t.Fatal("removing default should be refused: it is the base configuration, not a stored profile")
	}
}

func TestRemoveProviderRejectsUnknownAndEmpty(t *testing.T) {
	state := newTestState(t)
	if _, err := state.removeProviderProfile("ghost"); err == nil {
		t.Error("removing an unknown provider should fail rather than report success")
	}
	if _, err := state.removeProviderProfile("  "); err == nil {
		t.Error("an empty name should be rejected")
	}
}

func TestRemoveProviderPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	state := loadBridgeStateFromFile(path)
	if state.loadErr != nil {
		t.Fatalf("load: %v", state.loadErr)
	}
	if err := state.setProviderProfile("codex", config.ProviderProfile{Model: "gpt-5.6-luna"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := state.removeProviderProfile("codex"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Re-read from disk: a removal that only mutated memory would come back after
	// a restart.
	reloaded := loadBridgeStateFromFile(path)
	if reloaded.loadErr != nil {
		t.Fatalf("reload: %v", reloaded.loadErr)
	}
	if _, still := reloaded.providerProfiles()["codex"]; still {
		t.Error("removal did not persist to disk")
	}
}

// The active profile is the source of truth for which model answers, because the
// runtime override is empty unless the user set one.
func TestActiveProviderProfileReportsProfileModel(t *testing.T) {
	state := newTestState(t)
	if err := state.setProviderProfile("codex", config.ProviderProfile{Model: "gpt-5.6-luna"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := state.setProvider("codex"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	_, runtimeModel, _ := state.values()
	if runtimeModel != "" {
		t.Skip("runtime override is set, so this case no longer isolates the profile")
	}
	id, profile, ok := state.activeProviderProfile()
	if !ok {
		t.Fatal("no active profile found")
	}
	if id != "codex" || profile.Model != "gpt-5.6-luna" {
		t.Errorf("active profile = %s/%s, want codex/gpt-5.6-luna", id, profile.Model)
	}
}
