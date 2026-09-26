package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeRuntimeState points Ceraa at a temp data dir and drops a bridge-state.json
// into it, standing in for a provider chosen from the Telegram menu.
func writeRuntimeState(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "bridge-state.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The gap: a provider activated from Telegram was invisible to every other entry
// point, so the CLI reported no endpoint configured for a provider that was
// demonstrably working.
func TestActiveLLMFallsBackToTheRuntimeSelection(t *testing.T) {
	writeRuntimeState(t, `{
	  "provider": "codex",
	  "provider_profiles": {
	    "codex": {"url":"https://chatgpt.com/backend-api/codex","model":"gpt-5.6-luna","context_limit":200000,"subscription":"codex"}
	  }
	}`)
	cfg := DefaultConfig()
	// config.json knows nothing, which is the condition being fixed.
	if cfg.LLM.URL != "" {
		t.Fatal("precondition: the base config should name no endpoint")
	}
	got := cfg.ActiveLLM()
	if got.URL != "https://chatgpt.com/backend-api/codex" || got.Model != "gpt-5.6-luna" {
		t.Errorf("ActiveLLM = %+v, want the runtime profile", got)
	}
	if got.Subscription != "codex" {
		t.Errorf("subscription = %q, want codex", got.Subscription)
	}
	// The profile's explicit limit must survive, because it is a deliberate
	// conservative budget for an endpoint that advertises no window.
	if got.ContextLimit != 200000 {
		t.Errorf("context limit = %d, want 200000", got.ContextLimit)
	}
}

// A provider written in config.json is something the user typed and can see, so it
// wins over the runtime selection. Otherwise a later config edit would appear to
// do nothing.
func TestConfigProviderWinsOverTheRuntimeSelection(t *testing.T) {
	writeRuntimeState(t, `{
	  "provider": "codex",
	  "provider_profiles": {
	    "codex": {"url":"https://runtime.example","model":"runtime/model"}
	  }
	}`)
	cfg := DefaultConfig()
	cfg.LLM.URL = "https://config.example"
	cfg.LLM.Model = "config/model"

	got := cfg.ActiveLLM()
	if got.URL != "https://config.example" || got.Model != "config/model" {
		t.Errorf("ActiveLLM = %+v, want the config provider to win", got)
	}
}

// An explicitly named config profile also wins, even when the base LLM block is
// empty. ActiveProvider is the deliberate selector and outranks the fallback.
func TestNamedConfigProfileWinsOverTheRuntimeSelection(t *testing.T) {
	writeRuntimeState(t, `{
	  "provider": "codex",
	  "provider_profiles": {"codex": {"url":"https://runtime.example","model":"runtime/model"}}
	}`)
	cfg := DefaultConfig()
	cfg.ActiveProvider = "work"
	cfg.ProviderProfiles = map[string]ProviderProfile{
		"work": {URL: "https://work.example", Model: "work/model"},
	}
	got := cfg.ActiveLLM()
	if got.URL != "https://work.example" {
		t.Errorf("ActiveLLM url = %q, want https://work.example", got.URL)
	}
}

// A fresh install has no runtime state at all. That is a normal condition, not a
// failure, and must not surface as a parse error.
func TestNoRuntimeStateIsNotAnError(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	state, err := LoadRuntimeState()
	if err != nil {
		t.Fatalf("LoadRuntimeState: %v", err)
	}
	if len(state.ProviderProfiles) != 0 {
		t.Errorf("expected no profiles, got %v", state.ProviderProfiles)
	}
	cfg := DefaultConfig()
	if got := cfg.ActiveLLM(); got.URL != "" {
		t.Errorf("ActiveLLM = %+v, want the empty base", got)
	}
}

// A runtime profile with no endpoint cannot serve a request, so it must not be
// adopted and then fail later inside the transport.
func TestRuntimeProfileWithoutAnEndpointIsIgnored(t *testing.T) {
	writeRuntimeState(t, `{"provider":"broken","provider_profiles":{"broken":{"model":"some/model"}}}`)
	cfg := DefaultConfig()
	if got := cfg.ActiveLLM(); got.URL != "" {
		t.Errorf("ActiveLLM adopted an endpointless profile: %+v", got)
	}
}

// A named provider with no matching profile is a misconfiguration, and falling
// through to some other profile would hide it.
func TestRuntimeSelectionWithAnUnknownProviderFallsThrough(t *testing.T) {
	writeRuntimeState(t, `{"provider":"ghost","provider_profiles":{"other":{"url":"https://other.example"}}}`)
	cfg := DefaultConfig()
	if got := cfg.ActiveLLM(); got.URL != "" {
		t.Errorf("ActiveLLM = %+v, want nothing adopted for an unknown provider", got)
	}
}

// Unset fields in the runtime profile must keep the base value rather than
// blanking it. The two files are written by different code and populate different
// fields.
func TestRuntimeProfileMergeKeepsBaseFields(t *testing.T) {
	writeRuntimeState(t, `{"provider":"p","provider_profiles":{"p":{"url":"https://p.example","model":"m"}}}`)
	cfg := DefaultConfig()
	cfg.LLM.PromptCacheMode = "auto"
	cfg.LLM.MaxOutputTokens = 8192
	got := cfg.ActiveLLM()
	if got.PromptCacheMode != "auto" {
		t.Errorf("prompt cache mode = %q, want the base value auto", got.PromptCacheMode)
	}
	if got.MaxOutputTokens != 8192 {
		t.Errorf("max output tokens = %d, want the base value 8192", got.MaxOutputTokens)
	}
}

func TestLoadRuntimeStateRejectsMalformedJSON(t *testing.T) {
	writeRuntimeState(t, `{not json`)
	if _, err := LoadRuntimeState(); err == nil {
		t.Error("malformed runtime state parsed without error")
	}
}

// Reasoning sits beside the profile in the runtime state, not inside it, so it has
// to be adopted separately or the CLI runs at the provider default while the
// daemon runs at the level the user chose.
func TestRuntimeReasoningIsAdopted(t *testing.T) {
	writeRuntimeState(t, `{
	  "provider": "codex",
	  "reasoning": "high",
	  "provider_profiles": {"codex": {"url":"https://chatgpt.example","model":"m"}}
	}`)
	if got := DefaultConfig().ActiveLLM().Reasoning; got != "high" {
		t.Errorf("reasoning = %q, want high", got)
	}
}

func TestConfiguredReasoningIsNotOverridden(t *testing.T) {
	writeRuntimeState(t, `{
	  "provider": "codex",
	  "reasoning": "high",
	  "provider_profiles": {"codex": {"url":"https://chatgpt.example","model":"m","reasoning":"low"}}
	}`)
	if got := DefaultConfig().ActiveLLM().Reasoning; got != "low" {
		t.Errorf("reasoning = %q, want the profile value low", got)
	}
}
