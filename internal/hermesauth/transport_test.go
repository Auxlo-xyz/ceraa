package hermesauth

import (
	"context"
	"strings"
	"testing"

	"ceraa/harness/llm"
)

// A Hermes credential naming openai-codex or minimax-oauth was refused at startup
// with "native chat adapter is not available yet" - a sentence describing a gap
// rather than the state of the codebase, because both of those transports already
// exist in harness/llm. Only the chat one was reachable.
//
// These assert the credential's transport selects the transport, for all three.
func TestACredentialsTransportSelectsTheNativeTransport(t *testing.T) {
	cases := []struct {
		transport string
		check     func(llm.ProviderConfig) bool
		want      string
	}{
		{TransportCodexResponses, func(m llm.ProviderConfig) bool { return m.CodexNative && m.CodexTokens != nil }, "codex_responses"},
		{TransportAnthropicMessages, func(m llm.ProviderConfig) bool { return m.AnthropicNative && m.AnthropicTokens != nil }, "anthropic_messages"},
		{TransportChatCompletions, func(m llm.ProviderConfig) bool {
			return !m.CodexNative && !m.AnthropicNative
		}, "chat_completions"},
		{"", func(m llm.ProviderConfig) bool {
			// An absent transport is the chat one, which is what transportForProvider
			// has always returned for anything it does not recognise.
			return !m.CodexNative && !m.AnthropicNative
		}, "unset"},
	}

	for _, tc := range cases {
		credential := Credential{
			Provider:  "nous",
			Transport: tc.transport,
			BaseURL:   "https://inference.example/v1",
		}
		model, err := ApplyCredential(llm.ProviderConfig{Model: "some-model"}, credential, nil)
		if err != nil {
			t.Errorf("%s: %v", tc.want, err)
			continue
		}
		if !tc.check(model) {
			t.Errorf("%s: transport not applied, got CodexNative=%v AnthropicNative=%v",
				tc.want, model.CodexNative, model.AnthropicNative)
		}
		if model.BaseURL != credential.BaseURL {
			t.Errorf("%s: base URL = %q", tc.want, model.BaseURL)
		}
	}
}

// Both native transports depend on being able to renew the credential, because a
// 401 on either is only recoverable if a refresher is wired. That is the whole
// reason the Anthropic transport gained its recovery path, and a Hermes
// credential routed through it must not be the exception.
func TestNativeTransportsCarryARefresher(t *testing.T) {
	for _, transport := range []string{TransportCodexResponses, TransportAnthropicMessages} {
		credential := Credential{
			Provider: "nous", Transport: transport, BaseURL: "https://inference.example/v1",
		}
		model, err := ApplyCredential(llm.ProviderConfig{Model: "m"}, credential, nil)
		if err != nil {
			t.Fatalf("%s: %v", transport, err)
		}
		var source llm.CodexTokenSource
		switch transport {
		case TransportCodexResponses:
			source = model.CodexTokens
		case TransportAnthropicMessages:
			source = model.AnthropicTokens
		}
		if source == nil {
			t.Fatalf("%s: no token source wired", transport)
		}
		if _, ok := source.(llm.CodexRefresher); !ok {
			t.Errorf("%s: the token source cannot refresh, so a rejected credential is unrecoverable", transport)
		}
	}
}

// The Messages style is a property of the host, and the two hosts disagree in a
// way that produces a 401 naming the header the other one wanted.
func TestMessagesAuthStyleFollowsTheHost(t *testing.T) {
	if got := MessagesAuthForHost("https://agent.minimax.io/mavis/api/v1/llm/v1"); got != llm.MessagesAuthBearer {
		t.Errorf("the agent backend got style %q, want bearer", got)
	}
	for _, host := range []string{
		"https://api.minimax.io/anthropic",
		"https://api.anthropic.com",
		"https://inference.example/v1",
	} {
		if got := MessagesAuthForHost(host); got != "" {
			t.Errorf("%s got style %q, want the x-api-key default", host, got)
		}
	}
}

// A credential with no URL cannot be called at all, and saying so beats
// configuring a transport that will fail on the first request.
func TestACredentialWithoutABaseURLIsRefused(t *testing.T) {
	_, err := ApplyCredential(llm.ProviderConfig{Model: "m"}, Credential{Provider: "nous"}, nil)
	if err == nil {
		t.Fatal("a credential with no base URL was accepted")
	}
	if !strings.Contains(err.Error(), "base URL") {
		t.Errorf("error does not name the missing value: %v", err)
	}
}

// An unrecognised transport must fail loudly rather than silently falling through
// to the chat one and 401ing later.
func TestAnUnknownTransportIsRefusedByName(t *testing.T) {
	_, err := ApplyCredential(llm.ProviderConfig{Model: "m"}, Credential{
		Provider: "nous", Transport: "grpc_frames", BaseURL: "https://x.example/v1",
	}, nil)
	if err == nil {
		t.Fatal("an unknown transport was accepted")
	}
	if !strings.Contains(err.Error(), "grpc_frames") {
		t.Errorf("error does not name the transport: %v", err)
	}
}

// The two former call sites each carried the same refusal, which is why the gap
// existed twice. There is now one entry point, and it is the one both use.
func TestTheTransportChoiceLivesInOnePlace(t *testing.T) {
	// A credential the old code refused outright, resolved without error now.
	credential := Credential{
		Provider: "openai-codex", Transport: TransportCodexResponses,
		BaseURL: "https://chatgpt.com/backend-api/codex",
	}
	if _, err := ApplyCredential(llm.ProviderConfig{Model: "gpt-6.1-sol"}, credential, nil); err != nil {
		t.Errorf("a codex_responses credential is still refused: %v", err)
	}
}

// The refresher must actually reach the store rather than re-resolving inside the
// skew, which would hand back the same rejected token.
func TestTheTokenSourceRefreshesForced(t *testing.T) {
	source := &TokenSource{ProviderID: "nous", Store: &Store{Home: t.TempDir()}}
	// No credential on disk: both paths must report that rather than succeed
	// quietly, which is what a no-op refresh would look like.
	if token, err := source.AccessToken(context.Background()); err == nil {
		t.Errorf("AccessToken returned %q with no credential stored", token)
	}
	if token, err := source.RefreshToken(context.Background()); err == nil {
		t.Errorf("RefreshToken returned %q with no credential stored", token)
	}
}
