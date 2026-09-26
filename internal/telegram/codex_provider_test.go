package telegram

import (
	"context"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/config"
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
	resolve := providerModelResolver(provider)

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
	})
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
	})
	model, err := resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if model.CodexNative || model.CodexTokens != nil {
		t.Error("a plain custom provider was routed to the Codex transport")
	}
}
