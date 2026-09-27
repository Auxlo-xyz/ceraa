package telegram

import (
	"context"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/config"
	"ceraa/internal/telegram/menus"
)

// The provider menu offers Codex, so selecting it has to produce a config the
// transport will actually use. A profile that only carries a URL would silently
// fall through to the chat-completions adapter and fail at request time.
func TestProviderResolverWiresCodexTransport(t *testing.T) {
	provider := config.LLMProvider{
		URL:          llm.CodexBaseURL,
		Model:        llm.DefaultCodexModel,
		Subscription: llm.CodexSubscription,
	}
	resolve := providerModelResolver(provider, "tg-codex-transport")

	model, err := resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !model.CodexNative {
		t.Error("CodexNative not set, so the request would go to the chat-completions adapter")
	}
	if model.CodexTokens == nil {
		t.Error("CodexTokens not set, so the transport would refuse the request")
	}
	if model.BaseURL != llm.CodexBaseURL {
		t.Errorf("base URL = %q, want %q", model.BaseURL, llm.CodexBaseURL)
	}
}

// A profile that names the subscription but omits the URL still has to resolve
// to the Codex endpoint, because that is the only host it can work against.
func TestProviderResolverFillsMissingCodexURL(t *testing.T) {
	resolve := providerModelResolver(config.LLMProvider{
		Model:        llm.DefaultCodexModel,
		Subscription: llm.CodexSubscription,
	}, "tg-codex-url")
	model, err := resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if model.BaseURL != llm.CodexBaseURL {
		t.Errorf("base URL = %q, want it defaulted to %q", model.BaseURL, llm.CodexBaseURL)
	}
}

// A non-Codex subscription must not be dragged onto the Codex transport.
func TestProviderResolverLeavesOtherSubscriptionsAlone(t *testing.T) {
	resolve := providerModelResolver(config.LLMProvider{
		URL:   "https://openrouter.ai/api/v1",
		Model: "some/model",
	}, "tg-custom-provider")
	model, err := resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if model.CodexNative || model.CodexTokens != nil {
		t.Error("a plain custom provider was routed to the Codex transport")
	}
}

// The Provider / Model menu is the entry point a configured user actually sees;
// the onboarding menu is not shown again once a provider is set. A Codex entry
// that exists only in onboarding is unreachable, which is how it shipped.
func TestProviderModelMenuOffersCodex(t *testing.T) {
	markup := menus.Inline("provider", 0)
	rows, ok := markup["inline_keyboard"].([][]map[string]string)
	if !ok {
		t.Fatal("expected model inline keyboard")
	}
	var label string
	for _, row := range rows {
		for _, button := range row {
			if button["callback_data"] == "provider:codex" {
				label = button["text"]
			}
		}
	}
	if label == "" {
		t.Fatalf("Provider / Model menu has no Codex entry; callbacks present: %v", collectCallbacks(rows))
	}
	if !strings.Contains(label, "Codex") {
		t.Errorf("Codex button label = %q", label)
	}
}

func collectCallbacks(rows [][]map[string]string) []string {
	var out []string
	for _, row := range rows {
		for _, button := range row {
			out = append(out, button["callback_data"])
		}
	}
	return out
}

// The Context menu is how a user checks what is actually serving them, so it has
// to read the active profile. Reading the base config reported an empty model and
// the wrong context limit whenever the provider came from a profile.
func TestActiveProfileSuppliesModelAndContextLimit(t *testing.T) {
	state := newTestState(t)
	if err := state.setProviderProfile("codex", config.ProviderProfile{
		Model:        "gpt-5.6-luna",
		ContextLimit: 200000,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := state.setProvider("codex"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	_, profile, ok := state.activeProviderProfile()
	if !ok {
		t.Fatal("no active profile")
	}
	if profile.Model != "gpt-5.6-luna" {
		t.Errorf("model = %q, want the profile's, not the base config's", profile.Model)
	}
	if profile.ContextLimit != 200000 {
		t.Errorf("context limit = %d, want the profile's 200000", profile.ContextLimit)
	}
}
