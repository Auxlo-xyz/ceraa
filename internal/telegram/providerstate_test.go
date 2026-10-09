package telegram

import (
	"os"
	"path/filepath"
	"testing"

	"ceraa/internal/config"
)

// stubBothState points the bot at a temporary data directory for both halves of
// its state: the bridge state and the config that now holds the provider
// selection. It returns the restore func.
//
// Both are needed because they are two files in two packages that a test can
// reach independently, and a test that moved only one of them would be writing to
// or reading from the live install.
func stubBothState(t *testing.T) func() {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	previousState := defaultState
	defaultState = loadBridgeStateFromFile(filepath.Join(dir, "bridge-state.json"))
	previousProviders := providers
	providers = newProviderState(config.NewStoreAt(filepath.Join(dir, "config.json")))
	return func() {
		defaultState = previousState
		providers = previousProviders
	}
}

// withProviderState points the provider state at a temporary config.json seeded
// with a selection, and restores the previous one afterwards.
//
// Provider state no longer lives on bridgeState, so a test that wants a provider
// selected has to write config. Seeding a real file rather than a struct literal
// is deliberate: the whole point of the change is that the file is the state, and
// a test that bypassed it would not notice if the file stopped being read.
func withProviderState(t *testing.T, active string, profiles map[string]config.ProviderProfile) *config.Store {
	t.Helper()
	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	err := store.Update(func(cfg *config.Config) error {
		cfg.ActiveProvider = active
		cfg.ProviderProfiles = profiles
		return nil
	})
	if err != nil {
		t.Fatalf("seed provider state: %v", err)
	}
	previous := providers
	providers = newProviderState(store)
	t.Cleanup(func() { providers = previous })
	return store
}

// The selection must survive a round trip through the file, because the file is
// now the only place it exists.
func TestProviderSelectionLivesInConfigAndSurvivesARead(t *testing.T) {
	store := withProviderState(t, "minimax", map[string]config.ProviderProfile{
		"minimax": {URL: "https://api.minimax.io/anthropic", Model: "MiniMax-M3", Subscription: "minimax"},
		"xai":     {URL: "https://api.x.ai/v1", Model: "grok-4.6", Subscription: "xai"},
	})

	if got := providers.active(); got != "minimax" {
		t.Errorf("active provider = %q, want minimax", got)
	}
	if !providers.hasProfile("xai") {
		t.Error("the xai profile was not read back from config")
	}
	// A fresh Store over the same path is what a second process would see.
	other := config.NewStoreAt(store.Path())
	cfg, err := other.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActiveProvider != "minimax" {
		t.Errorf("a second reader saw active provider %q, want minimax", cfg.ActiveProvider)
	}
	if len(cfg.ProviderProfiles) != 2 {
		t.Errorf("a second reader saw %d profiles, want 2", len(cfg.ProviderProfiles))
	}
}

// Switching providers must be readable by the very next reader, with no cached
// copy to disagree with. This is the failure that made a run report one provider
// while the menu reported another.
func TestSwitchingProvidersIsVisibleToTheNextRead(t *testing.T) {
	withProviderState(t, "xai", map[string]config.ProviderProfile{
		"xai":     {URL: "https://api.x.ai/v1", Model: "grok-4.6", Subscription: "xai"},
		"minimax": {URL: "https://api.minimax.io/anthropic", Model: "MiniMax-M3", Subscription: "minimax"},
	})

	if got := providers.active(); got != "xai" {
		t.Fatalf("precondition: active = %q", got)
	}
	if err := providers.setActive("minimax"); err != nil {
		t.Fatal(err)
	}
	if got := providers.active(); got != "minimax" {
		t.Errorf("after switching, active = %q, want minimax", got)
	}
	id, profile, ok := providers.activeProfile()
	if !ok || id != "minimax" {
		t.Fatalf("active profile = %q ok=%v", id, ok)
	}
	if profile.URL != "https://api.minimax.io/anthropic" {
		t.Errorf("active profile URL = %q", profile.URL)
	}
}

// Removing the profile of the active provider must move the selection, or the
// next run points at a profile that no longer exists.
func TestRemovingTheActiveProfileMovesTheSelection(t *testing.T) {
	withProviderState(t, "minimax", map[string]config.ProviderProfile{
		"minimax": {URL: "https://api.minimax.io/anthropic", Model: "MiniMax-M3", Subscription: "minimax"},
	})
	removed, err := providers.removeProfile("minimax")
	if err != nil || !removed {
		t.Fatalf("removeProfile = %v, %v", removed, err)
	}
	if got := providers.active(); got != "default" {
		t.Errorf("active after removal = %q, want default", got)
	}
	if _, err := providers.removeProfile("default"); err == nil {
		t.Error("the default provider was removable, which would leave no provider at all")
	}
}

// The model, reasoning level and agent mode moved into the same file, so a single
// read answers all three and they cannot drift apart.
func TestModelReasoningAndAgentShareOneDocument(t *testing.T) {
	withProviderState(t, "minimax", map[string]config.ProviderProfile{
		"minimax": {URL: "https://api.minimax.io/anthropic", Model: "MiniMax-M3", Subscription: "minimax"},
	})
	if err := providers.setModel("MiniMax-M3"); err != nil {
		t.Fatal(err)
	}
	if err := providers.setReasoning("high"); err != nil {
		t.Fatal(err)
	}
	if err := providers.setAgent("build"); err != nil {
		t.Fatal(err)
	}
	if err := providers.setAutoCompaction(true); err != nil {
		t.Fatal(err)
	}

	model, agent := providers.modelAndAgent()
	if model != "MiniMax-M3" || agent != "build" {
		t.Errorf("model/agent = %q/%q", model, agent)
	}
	if got := providers.reasoning(); got != "high" {
		t.Errorf("reasoning = %q", got)
	}
	enabled, set := providers.autoCompaction()
	if !enabled || !set {
		t.Errorf("autoCompaction = %v, set=%v", enabled, set)
	}
}

// A legacy bridge-state.json is folded into config rather than stranded, and only
// when it really carries the old layout.
func TestLegacySelectionIsAdoptedOnLoad(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "bridge-state.json")
	legacy := `{"provider":"minimax","model":"MiniMax-M3","agent":"build","reasoning":"high",
		"auto_compaction":true,"auto_compaction_set":true,
		"provider_profiles":{"minimax":{"url":"https://api.minimax.io/anthropic","model":"MiniMax-M3","subscription":"minimax"}},
		"active_sessions":{"1":"s1"}}`
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store := config.NewStoreAt(filepath.Join(dir, "config.json"))
	previous := providers
	providers = newProviderState(store)
	t.Cleanup(func() { providers = previous })

	state := loadBridgeStateFromFile(legacyPath)
	if state.loadErr != nil {
		t.Fatalf("load reported %v", state.loadErr)
	}
	if got := providers.active(); got != "minimax" {
		t.Errorf("migrated active provider = %q, want minimax", got)
	}
	if !providers.hasProfile("minimax") {
		t.Error("the legacy provider profile was not migrated")
	}
	if got := providers.reasoning(); got != "high" {
		t.Errorf("migrated reasoning = %q", got)
	}
	// The bot's own state is still read from the file it belongs in.
	if state.ActiveSessions["1"] != "s1" {
		t.Error("active_sessions was not read from bridge state")
	}
}

// A bridge-state.json carrying none of the retired provider keys is not evidence
// of an older layout, and must not overwrite the real config.
func TestBridgeStateWithoutRetiredKeysDoesNotOverwriteConfig(t *testing.T) {
	dir := t.TempDir()
	store := config.NewStoreAt(filepath.Join(dir, "config.json"))
	if err := store.Update(func(cfg *config.Config) error {
		cfg.ActiveProvider = "xai"
		cfg.ProviderProfiles = map[string]config.ProviderProfile{
			"xai": {URL: "https://api.x.ai/v1", Model: "grok-4.6", Subscription: "xai"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	previous := providers
	providers = newProviderState(store)
	t.Cleanup(func() { providers = previous })

	legacyPath := filepath.Join(dir, "bridge-state.json")
	if err := os.WriteFile(legacyPath, []byte(`{"active_sessions":{"1":"s1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loadBridgeStateFromFile(legacyPath)

	if got := providers.active(); got != "xai" {
		t.Errorf("active provider = %q; a bot-only file was treated as the old layout", got)
	}
}

// A real install only writes the fields the user actually set, so a legacy file
// carrying a subset of the retired keys is the normal case, not a partial one.
// Requiring all of them dropped the selection and left the bot on the default
// provider with no error anywhere.
func TestPartialLegacyLayoutStillMigrates(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "bridge-state.json")
	legacy := `{"provider":"minimax","agent":"build","active_sessions":{"1":"s1"},
		"always_allow":{"bash":true},
		"provider_profiles":{"minimax":{"url":"https://api.minimax.io/anthropic","model":"MiniMax-M3","subscription":"minimax"}}}`
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store := config.NewStoreAt(filepath.Join(dir, "config.json"))
	previous := providers
	providers = newProviderState(store)
	t.Cleanup(func() { providers = previous })

	state := loadBridgeStateFromFile(legacyPath)
	if state.loadErr != nil {
		t.Fatalf("load reported %v", state.loadErr)
	}
	if got := providers.active(); got != "minimax" {
		t.Errorf("migrated active provider = %q, want minimax", got)
	}
	if !providers.hasProfile("minimax") {
		t.Error("the legacy provider profile was not migrated")
	}
	if _, agent := providers.modelAndAgent(); agent != "build" {
		t.Errorf("migrated agent = %q, want build", agent)
	}
	// The bot's own state still comes from the file it belongs in.
	if state.ActiveSessions["1"] != "s1" || !state.AlwaysAllow["bash"] {
		t.Error("bot runtime state was not read from bridge state")
	}
}

// stubProviders points only the provider state at a temporary config, for tests
// that manage the bridge state themselves.
//
// Both halves have to move together. A test that changes CERAA_HOME but leaves the
// provider store alone still writes through the package-level store, and the next
// test then reads that write and sees a provider it never set — which is how a
// "no provider configured" assertion started failing because a test three cases
// earlier had activated one.
func stubProviders(t *testing.T) func() {
	t.Helper()
	dir := os.Getenv("CERAA_HOME")
	if dir == "" {
		dir = t.TempDir()
		t.Setenv("CERAA_HOME", dir)
	}
	previous := providers
	providers = newProviderState(config.NewStoreAt(filepath.Join(dir, "config.json")))
	return func() { providers = previous }
}

// A stale config token must never shadow a working environment one. The live
// incident: config held a placeholder while the environment held the real
// token, the registry read config first, and send_media answered 401 on a
// chat the bot itself could talk on.
func TestMediaTokenPrefersEnvironmentOverConfig(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "env-token")
	cfg := config.DefaultConfig()
	cfg.Telegram.BotToken = "config-token"
	if got := resolveMediaToken(cfg); got != "env-token" {
		t.Errorf("media token = %q, want the environment value", got)
	}
}

// With no environment token the config value is what there is.
func TestMediaTokenFallsBackToConfig(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	cfg := config.DefaultConfig()
	cfg.Telegram.BotToken = "config-token"
	if got := resolveMediaToken(cfg); got != "config-token" {
		t.Errorf("media token = %q, want the config value", got)
	}
}
