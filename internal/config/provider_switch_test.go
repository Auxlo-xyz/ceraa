package config

import (
	"path/filepath"
	"testing"
)

// Switching a provider has to take effect for the next turn, not the next restart.
// The two stores disagree by design — config.json is the single source of truth
// and the runtime selection is a fallback — so the order they are consulted in is
// the whole question.
func TestEffectiveProviderIDFollowsAnImmediateSwitch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	store := NewStoreAt(filepath.Join(dir, "config.json"))
	if err := store.Update(func(c *Config) error {
		c.ActiveProvider = "xai"
		return nil
	}); err != nil {
		t.Fatalf("seed xai: %v", err)
	}
	if got := effective(); got != "xai" {
		t.Fatalf("after seeding xai, EffectiveProviderID = %q", got)
	}
	if err := store.Update(func(c *Config) error {
		c.ActiveProvider = "minimax"
		return nil
	}); err != nil {
		t.Fatalf("switch to minimax: %v", err)
	}
	if got := effective(); got != "minimax" {
		t.Errorf("after switching to minimax, EffectiveProviderID = %q; the switch needs a restart to apply", got)
	}
}

// effective loads the config and asks the same question the runner asks.
func effective() string {
	cfg, err := Load()
	if err != nil {
		return "<load error: " + err.Error() + ">"
	}
	return cfg.EffectiveProviderID()
}
