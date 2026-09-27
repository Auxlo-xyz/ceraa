package telegram

import (
	"testing"

	"ceraa/internal/config"
)

// A subscription provider keeps its credential in the encrypted store rather than
// in its profile, so requiring a profile API key reported a working Codex install
// as unconfigured. The consequence was not cosmetic: /start answered with the
// onboarding picker and startup logged that onboarding was still required.
func TestProviderConfiguredAcceptsASubscriptionWithoutAKey(t *testing.T) {
	previous := defaultState
	defaultState = &bridgeState{Provider: "codex", ProviderProfiles: map[string]config.ProviderProfile{
		"codex": {URL: "https://chatgpt.com/backend-api/codex", Model: "gpt-5.6-luna", Subscription: "codex"},
	}}
	t.Cleanup(func() { defaultState = previous })

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
	previous := defaultState
	defaultState = &bridgeState{Provider: "custom", ProviderProfiles: map[string]config.ProviderProfile{
		"custom": {URL: "https://openrouter.ai/api/v1", Model: "some/model"},
	}}
	t.Cleanup(func() { defaultState = previous })

	cfg := config.DefaultConfig()
	cfg.ProviderProfiles = map[string]config.ProviderProfile{
		"custom": {URL: "https://openrouter.ai/api/v1", Model: "some/model"},
	}
	cfg.ActiveProvider = "custom"
	if providerConfigured(cfg) {
		t.Error("a plain provider with no API key was reported as configured")
	}
}
