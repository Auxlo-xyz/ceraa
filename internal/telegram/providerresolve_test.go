package telegram

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/clineauth"
	"ceraa/internal/config"
	"ceraa/internal/hermesauth"
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

// The Nous Portal's inference host sits behind a WAF that answers 403 with a
// Cloudflare page for a default Go client, so the resolved model must carry the
// Hermes client identity or every run fails on a web page.
func TestTelegramResolverGivesHermesItsClientIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}
	store := hermesauth.NewStore()
	store.Home = filepath.Join(dir, "hermes")
	if err := os.MkdirAll(store.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Home, "auth.json"), []byte(`{
		"active_provider": "nous",
		"providers": {"nous": {
			"access_token": "hermes-token",
			"refresh_token": "hermes-refresh",
			"base_url": "https://inference.example/v1",
			"expires_at_ms": 4102444800000
		}}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERMES_HOME", store.Home)

	profile := config.LLMProvider{
		URL: llm.HermesBaseURL, Model: llm.DefaultHermesModel,
		Subscription: llm.HermesSubscription, HermesProvider: "nous",
	}
	model, err := providerModelResolver(profile, "tg-hermes-ua")(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if model.ExtraHeaders["User-Agent"] != hermesauth.ClientUserAgent {
		t.Errorf("User-Agent = %q, want %q; the WAF would answer 403", model.ExtraHeaders["User-Agent"], hermesauth.ClientUserAgent)
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
