package hermesauth

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/llm"
)

// The transports a Hermes credential may name.
//
// A Hermes credential is not always an OpenAI-shaped chat endpoint. openai-codex
// speaks the Responses API and minimax-oauth speaks the Anthropic Messages API,
// and both of those transports already exist in harness/llm. Only the chat one
// was reachable, so a credential naming either of the others was refused at
// startup with a message saying the adapter was unavailable - which described
// this gap rather than the state of the codebase.
const (
	TransportChatCompletions   = "chat_completions"
	TransportCodexResponses    = "codex_responses"
	TransportAnthropicMessages = "anthropic_messages"
)

// TokenSource adapts a Hermes credential to the credential interface the llm
// transports expect, so the transports do not know how Hermes stores anything.
//
// It satisfies both CodexTokenSource and CodexRefresher, which is what makes a
// token rejected before its recorded expiry recoverable on any of the three
// transports: a 401 forces one exchange and replays the request once.
type TokenSource struct {
	Store      *Store
	ProviderID string
}

func (t *TokenSource) store() *Store {
	if t.Store != nil {
		return t.Store
	}
	return NewStore()
}

// AccessToken returns the current credential. Resolve already refreshes inside the
// store's skew, so this is not a raw read of a possibly-expired value.
func (t *TokenSource) AccessToken(ctx context.Context) (string, error) {
	credential, err := t.store().Resolve(ctx, t.ProviderID)
	if err != nil {
		return "", err
	}
	return credential.AccessToken, nil
}

// RefreshToken forces one exchange and returns the new credential.
//
// Deliberately forced rather than relying on Resolve: the reason the transport is
// calling this is that Resolve handed back a credential the endpoint rejected, so
// resolving again inside the skew would return the same rejected token.
func (t *TokenSource) RefreshToken(ctx context.Context) (string, error) {
	store := t.store()
	if err := store.Refresh(ctx, t.ProviderID); err != nil {
		return "", err
	}
	credential, err := store.Resolve(ctx, t.ProviderID)
	if err != nil {
		return "", err
	}
	return credential.AccessToken, nil
}

// MessagesAuthForHost decides which header the Messages transport authenticates
// with.
//
// The style is a property of the host, not a preference, and the two disagree
// loudly: MiniMax's agent backend answers an x-api-key request with
// {"code":401,"message":"token is required"}, while the Anthropic-shaped platform
// API answers a bearer token with a request for an API secret key. Each error
// names the header the other one wanted, which is why this is decided by host
// rather than left to a default that is wrong half the time.
func MessagesAuthForHost(baseURL string) string {
	host := strings.ToLower(baseURL)
	if strings.Contains(host, "agent.minimax.io") {
		return llm.MessagesAuthBearer
	}
	// Anything else is the Anthropic-shaped surface, which wants x-api-key.
	return ""
}

// ApplyCredential configures a provider config from a resolved Hermes credential.
//
// This is the single place the three transports are chosen, because there were two
// copies of this logic - one in the CLI entry point and one in the bridge - and
// both carried the same refusal. The duplication is why the gap existed twice.
func ApplyCredential(model llm.ProviderConfig, credential Credential, store *Store) (llm.ProviderConfig, error) {
	if strings.TrimSpace(credential.BaseURL) == "" {
		return model, fmt.Errorf("hermes provider %q has no inference base URL", credential.Provider)
	}

	source := &TokenSource{Store: store, ProviderID: credential.Provider}
	model.BaseURL = credential.BaseURL
	model.APIKey = credential.AccessToken
	// Without this, Go's default User-Agent draws a Cloudflare 403 from the Nous
	// Portal's inference host, so the run dies on a web page instead of reaching
	// the API. A profile's own headers win.
	model.ExtraHeaders = WithClientHeaders(model.ExtraHeaders)

	switch credential.Transport {
	case TransportCodexResponses:
		// Speaks the Responses API, which is also what xAI uses; only where the
		// credential comes from differs, so it shares the transport.
		model.CodexNative = true
		model.CodexTokens = source
	case TransportAnthropicMessages:
		model.AnthropicNative = true
		model.AnthropicTokens = source
		model.MessagesAuthStyle = MessagesAuthForHost(credential.BaseURL)
	case "", TransportChatCompletions:
		// The API key above is the credential for this transport.
	default:
		return model, fmt.Errorf("hermes provider %q names an unknown transport %q", credential.Provider, credential.Transport)
	}

	// The transport depends on the resolved host, which the native adapters change.
	model.GeminiNative = llm.UsesGeminiNative(model.Model, model.BaseURL)
	model.GeminiThoughtSignatures = llm.UsesGeminiThoughtSignatures(model.Model, model.BaseURL)
	return model, nil
}
