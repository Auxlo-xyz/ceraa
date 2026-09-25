package config

import (
	"os"
	"path/filepath"
	"testing"
)

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
	if filepath.Base(cfg.WorkspaceRootPath()) != "workspace" {
		t.Fatalf("workspace root = %q", cfg.WorkspaceRootPath())
	}
}

func TestActiveLLMUsesSelectedProviderProfile(t *testing.T) {
	cfg := Config{
		LLM:            LLMProvider{URL: "default-url", APIKey: "default-key", Model: "default-model", ContextLimit: 100, MaxOutputTokens: 10},
		ActiveProvider: "fast",
		ProviderProfiles: map[string]ProviderProfile{
			"fast": {URL: "fast-url", APIKey: "fast-key", Model: "fast-model", ContextLimit: 200, MaxOutputTokens: 20, PromptCacheMode: "explicit", PromptCacheTTL: "1h"},
		},
	}
	active := cfg.ActiveLLM()
	if active.URL != "fast-url" || active.APIKey != "fast-key" || active.Model != "fast-model" || active.ContextLimit != 200 || active.MaxOutputTokens != 20 || active.PromptCacheMode != "explicit" || active.PromptCacheTTL != "1h" {
		t.Fatalf("unexpected active provider: %+v", active)
	}
}

func TestDefaultProviderFallsBackToLLMConfig(t *testing.T) {
	cfg := Config{LLM: LLMProvider{URL: "url", Model: "model"}, ActiveProvider: "missing"}
	if active := cfg.ActiveLLM(); active.URL != "url" || active.Model != "model" {
		t.Fatalf("unexpected fallback provider: %+v", active)
	}
}

// TestCeraaEnvIsCanonical pins that CERAA_* wins and SOORAA_* still works, so a
// deployment that has not been updated keeps functioning after the rename.
func TestCeraaEnvIsCanonical(t *testing.T) {
	t.Setenv("CERAA_MODEL", "ceraa-model")
	t.Setenv("SOORAA_MODEL", "legacy-model")
	if got := env("MODEL"); got != "ceraa-model" {
		t.Fatalf("env(MODEL) = %q, want the Ceraa value", got)
	}
	t.Setenv("CERAA_MODEL", "")
	if got := env("MODEL"); got != "legacy-model" {
		t.Fatalf("env(MODEL) = %q, want the legacy fallback", got)
	}
	t.Setenv("SOORAA_MODEL", "")
	if got := env("MODEL"); got != "" {
		t.Fatalf("env(MODEL) = %q, want empty", got)
	}
}

// TestUnmovedLegacyConfigIsStillFound pins the upgrade path: a config.json
// written before the rename must keep loading.
func TestUnmovedLegacyConfigIsStillFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CERAA_HOME", "")
	legacyDir := filepath.Join(home, ".config", "sooraa")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"llm":{"url":"https://legacy.example","model":"legacy-model","api_key":"legacy-key","context_limit":1234}}`
	if err := os.WriteFile(filepath.Join(legacyDir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Model != "legacy-model" || cfg.LLM.URL != "https://legacy.example" {
		t.Fatalf("legacy config was not loaded: %+v", cfg.LLM)
	}
}
