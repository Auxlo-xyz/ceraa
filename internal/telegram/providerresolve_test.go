package telegram

import (
	"context"
	"path/filepath"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/clineauth"
	"ceraa/internal/config"
	"ceraa/internal/minimaxauth"
	"ceraa/internal/secret"
	"ceraa/internal/xaiauth"
)

// A resolver that produces an empty API key for a subscription provider is the
// failure, whatever the transport: the request goes out unauthenticated and the
// provider answers 401. Asserted per provider with a credential actually stored.
func TestTelegramResolverAuthenticatesEachSubscription(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}

	// Store a credential for each, the way its login would.
	if err := xaiauth.Save(xaiauth.Credential{AccessToken: "xai-token"}); err != nil {
		t.Fatalf("xai: %v", err)
	}
	if err := minimaxauth.Save(minimaxauth.Credential{AccessToken: "mm-token", Region: minimaxauth.RegionGlobal}); err != nil {
		t.Fatalf("minimax: %v", err)
	}
	if err := clineauth.Save(clineauth.Credential{AccessToken: "cline-token"}); err != nil {
		t.Fatalf("cline: %v", err)
	}

	cases := []struct {
		providerID   string
		subscription string
		wantKey      string
	}{
		{"xai", llm.XAISubscription, "xai-token"},
		{"minimax", llm.MiniMaxSubscription, "mm-token"},
		{"cline", llm.ClineSubscription, "workos:cline-token"},
	}
	for _, tc := range cases {
		t.Run(tc.providerID, func(t *testing.T) {
			profile := config.LLMProvider{
				URL: llm.ClineGatewayBase, Model: "some-model", Subscription: tc.subscription,
			}
			model, err := providerModelResolver(profile, "tg-"+tc.providerID)(context.Background())
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if model.APIKey != tc.wantKey {
				t.Errorf("API key = %q, want %q; the request would be unauthenticated", model.APIKey, tc.wantKey)
			}
		})
	}
}

// The gateway headers have to be on the Cline model the bot actually builds.
func TestTelegramClineModelCarriesTheGatewayHeaders(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}
	if err := clineauth.Save(clineauth.Credential{AccessToken: "cline-token"}); err != nil {
		t.Fatal(err)
	}
	profile := config.LLMProvider{
		URL: llm.ClineGatewayBase, Model: "cline-free/deepseek-v4.1-flash",
		Subscription: llm.ClineSubscription,
	}
	model, err := providerModelResolver(profile, "tg-cline-headers")(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if model.APIKey != "workos:cline-token" {
		t.Errorf("API key = %q; the gateway requires the WorkOS marker", model.APIKey)
	}
	if !clineauth.HasClineHeaders(model.ExtraHeaders) {
		t.Fatalf("no gateway headers: %v", model.ExtraHeaders)
	}
	if model.ExtraHeaders["X-Task-ID"] != "tg-cline-headers" {
		t.Errorf("X-Task-ID = %q, want the Telegram session", model.ExtraHeaders["X-Task-ID"])
	}
	for _, key := range []string{"User-Agent", "X-Title", "X-CLIENT-TYPE", "X-Task-ID"} {
		if model.ExtraHeaders[key] == "" {
			t.Errorf("%s is empty", key)
		}
	}
}
