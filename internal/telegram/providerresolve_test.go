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
	"ceraa/internal/opencodeauth"
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

// Zen answers a request with no gateway headers, and the contract is the whole
// reason this provider exists as a first-class one rather than as a plain URL
// plus key. The resolver has to carry it, and it has to name Ceraa rather than
// the OpenCode CLI.
func TestTelegramResolverGivesOpenCodeItsHeaderContract(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}
	if err := opencodeauth.Save(opencodeauth.Credential{APIKey: "zk-opencode", ProjectID: "ws-7"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	profile := config.LLMProvider{
		URL: llm.OpenCodeBaseURL, Model: llm.DefaultOpenCodeModel,
		Subscription: llm.OpenCodeSubscription,
	}
	model, err := providerModelResolver(profile, "tg-opencode")(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if model.APIKey != "zk-opencode" {
		t.Errorf("API key = %q, want zk-opencode; the request would be unauthenticated", model.APIKey)
	}
	if model.BaseURL != llm.OpenCodeBaseURL {
		t.Errorf("BaseURL = %q, want %q", model.BaseURL, llm.OpenCodeBaseURL)
	}
	if !opencodeauth.HasOpenCodeHeaders(model.ExtraHeaders) {
		t.Errorf("ExtraHeaders = %v, missing the gateway contract", model.ExtraHeaders)
	}
	headers := model.ExtraHeaders
	if headers[opencodeauth.HeaderClient] != "ceraa" {
		t.Errorf("%s = %q, want ceraa; this program is Ceraa, not the OpenCode CLI",
			opencodeauth.HeaderClient, headers[opencodeauth.HeaderClient])
	}
	if headers[opencodeauth.HeaderProject] != "ws-7" {
		t.Errorf("%s = %q, want ws-7 from the stored credential", opencodeauth.HeaderProject, headers[opencodeauth.HeaderProject])
	}
	if model.ExtraHeadersFunc == nil {
		t.Fatal("ExtraHeadersFunc is nil; x-opencode-request would never be issued per request")
	}
	// The per-request id has to actually differ between calls, or it correlates
	// nothing and the static map would have been enough.
	first := model.ExtraHeadersFunc("ses-a")[opencodeauth.HeaderRequest]
	second := model.ExtraHeadersFunc("ses-a")[opencodeauth.HeaderRequest]
	if first == "" || first == second {
		t.Errorf("request id = %q then %q, want two distinct values", first, second)
	}
	perRequest := model.ExtraHeadersFunc("ses-a")
	if perRequest[opencodeauth.HeaderSession] != opencodeauth.GatewaySessionID("ses-a") {
		t.Errorf("the per-request builder did not render the session it was given into opencode's shape: %v", perRequest)
	}
	if perRequest[opencodeauth.HeaderAffinity] != "ses-a" {
		t.Errorf("%s = %q, want the real conversation ses-a", opencodeauth.HeaderAffinity, perRequest[opencodeauth.HeaderAffinity])
	}
}

// A profile that already carries an api_key resolves without anything in the
// encrypted store, so a hand-written config works and onboarding is not the only
// way in.
func TestTelegramResolverAcceptsOpenCodeFromTheProfileKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}
	profile := config.LLMProvider{
		URL: llm.OpenCodeBaseURL, Model: llm.DefaultOpenCodeModel,
		Subscription: llm.OpenCodeSubscription, APIKey: "zk-profile",
	}
	model, err := providerModelResolver(profile, "tg-opencode-profile")(context.Background())
	if err != nil {
		t.Fatalf("resolve with an empty store and a profile key: %v", err)
	}
	if model.APIKey != "zk-profile" {
		t.Errorf("API key = %q, want zk-profile", model.APIKey)
	}
	if !opencodeauth.HasOpenCodeHeaders(model.ExtraHeaders) {
		t.Errorf("ExtraHeaders = %v, the contract was not applied on the profile path", model.ExtraHeaders)
	}
}

// A provider onboarded with a store-held key must send that key, not the
// base provider's. The custom-provider incident: verification uses the pasted
// key and passes, while every real run inherited the base (Atria) key and
// failed 401 with a valid key sitting in the store.
func TestActiveProviderConfigPrefersStoreKeyOverBaseKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}
	if err := secret.Set("provider_api_key_custom", "custom-key"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The global state may hold a real custom profile; keep it out of the
	// way and put it back afterwards.
	prior, hadPrior := defaultState.providerProfiles()["custom"]
	if hadPrior {
		if _, err := defaultState.removeProviderProfile("custom"); err != nil {
			t.Fatalf("clear: %v", err)
		}
	}
	defer func() {
		if hadPrior {
			_ = defaultState.setProviderProfile("custom", prior)
		} else {
			_, _ = defaultState.removeProviderProfile("custom")
		}
	}()
	cfg := &config.Config{
		LLM: config.LLMProvider{URL: "https://api.atria-asi.ai/v1", Model: "m", APIKey: "base-key"},
		ProviderProfiles: map[string]config.ProviderProfile{
			"custom": {URL: "https://discovery-api.intern-ai.org.cn/v1", Model: "deepseek-v4-flash-0731"},
		},
	}
	got := activeProviderConfig(cfg, "custom")
	if got.APIKey != "custom-key" {
		t.Errorf("API key = %q, want the store-held custom key; runs would send the base key and fail 401", got.APIKey)
	}
	if got.Model != "deepseek-v4-flash-0731" {
		t.Errorf("model = %q, want the profile's", got.Model)
	}
	// A profile that names its own key keeps it: the store is a fallback,
	// not an override.
	cfg.ProviderProfiles["custom"] = config.ProviderProfile{URL: "https://discovery-api.intern-ai.org.cn/v1", Model: "m", APIKey: "profile-key"}
	if got := activeProviderConfig(cfg, "custom"); got.APIKey != "profile-key" {
		t.Errorf("API key = %q, want the profile's own key", got.APIKey)
	}
}
