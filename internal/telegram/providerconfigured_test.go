package telegram

import (
	"path/filepath"
	"testing"

	"ceraa/internal/config"
	"ceraa/internal/secret"
)

// A subscription provider keeps its credential in the encrypted store rather than
// in its profile, so requiring a profile API key reported a working Codex install
// as unconfigured. The consequence was not cosmetic: /start answered with the
// onboarding picker and startup logged that onboarding was still required.
func TestProviderConfiguredAcceptsASubscriptionWithoutAKey(t *testing.T) {
	withProviderState(t, "codex", map[string]config.ProviderProfile{
		"codex": {URL: "https://chatgpt.com/backend-api/codex", Model: "gpt-5.6-luna", Subscription: "codex"},
	})

	cfg := config.DefaultConfig()
	cfg.ProviderProfiles = map[string]config.ProviderProfile{
		"codex": {URL: "https://chatgpt.com/backend-api/codex", Model: "gpt-5.6-luna", Subscription: "codex"},
	}
	cfg.ActiveProvider = "codex"
	// No APIKey anywhere, which is exactly the subscription shape.
	if !providerConfigured(cfg) {
		t.Error("a working Codex provider was reported as unconfigured, so /start would send the user back to onboarding")
	}
}

func TestProviderConfiguredRejectsAPlainProviderWithoutAKey(t *testing.T) {
	withProviderState(t, "custom", map[string]config.ProviderProfile{
		"custom": {URL: "https://openrouter.ai/api/v1", Model: "some/model"},
	})

	cfg := config.DefaultConfig()
	cfg.ProviderProfiles = map[string]config.ProviderProfile{
		"custom": {URL: "https://openrouter.ai/api/v1", Model: "some/model"},
	}
	cfg.ActiveProvider = "custom"
	if providerConfigured(cfg) {
		t.Error("a plain provider with no API key was reported as configured")
	}
}

// A custom profile keeps its key in the secret store rather than the profile,
// and named profiles no longer inherit the base key. Without the store lookup
// a working custom install reads as unconfigured and /start loops the user
// back to onboarding forever.
func TestProviderConfiguredAcceptsAStoreHeldCustomKey(t *testing.T) {
	withProviderState(t, "custom", map[string]config.ProviderProfile{
		"custom": {URL: "https://discovery-api.intern-ai.org.cn/v1", Model: "deepseek-v4-flash-0731"},
	})
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}
	if err := secret.Set("provider_api_key_custom", "custom-key"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.ActiveProvider = "custom"
	if !providerConfigured(cfg) {
		t.Error("a custom provider with a store-held key was reported as unconfigured")
	}
}
