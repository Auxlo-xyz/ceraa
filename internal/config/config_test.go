package config

import "testing"

func TestDefaultTitleSettingsUseGeminiFlashLite(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.TitleModel != "gemini-3.5-flash-lite" {
		t.Fatalf("title model = %q", cfg.TitleModel)
	}
	if cfg.TitleAPIURL == "" {
		t.Fatal("title API URL is empty")
	}
}

func TestDefaultWorkspaceRootIsExplicit(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.WorkspaceRoot == "" || cfg.WorkspaceRootPath() == "" {
		t.Fatal("workspace root is empty")
	}
}

func TestActiveLLMUsesSelectedProviderProfile(t *testing.T) {
	cfg := Config{
		LLM:            LLMProvider{URL: "default-url", APIKey: "default-key", Model: "default-model", ContextLimit: 100, MaxOutputTokens: 10},
		ActiveProvider: "fast",
		ProviderProfiles: map[string]ProviderProfile{
			"fast": {URL: "fast-url", APIKey: "fast-key", Model: "fast-model", ContextLimit: 200, MaxOutputTokens: 20},
		},
	}
	active := cfg.ActiveLLM()
	if active.URL != "fast-url" || active.APIKey != "fast-key" || active.Model != "fast-model" || active.ContextLimit != 200 || active.MaxOutputTokens != 20 {
		t.Fatalf("unexpected active provider: %+v", active)
	}
}

func TestDefaultProviderFallsBackToLLMConfig(t *testing.T) {
	cfg := Config{LLM: LLMProvider{URL: "url", Model: "model"}, ActiveProvider: "missing"}
	if active := cfg.ActiveLLM(); active.URL != "url" || active.Model != "model" {
		t.Fatalf("unexpected fallback provider: %+v", active)
	}
}
