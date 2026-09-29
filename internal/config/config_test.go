package config

import (
	"encoding/json"
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

// TestEnvReadsCeraaPrefix pins the single supported variable prefix.
func TestEnvReadsCeraaPrefix(t *testing.T) {
	t.Setenv("CERAA_MODEL", "ceraa-model")
	if got := env("MODEL"); got != "ceraa-model" {
		t.Fatalf("env(MODEL) = %q", got)
	}
	t.Setenv("CERAA_MODEL", "")
	if got := env("MODEL"); got != "" {
		t.Fatalf("env(MODEL) = %q, want empty when unset", got)
	}
}

// The workspace is where the agent's commands run, so a path that no longer
// exists is not a degraded default, it is a wrong one: every relative path
// resolves to nothing and the agent reports an empty workspace while the live
// config stores a build directory. The Go toolchain puts a test binary under the
// temp directory, so deriving the root from os.Executable() without checking
// where that is produces exactly this.
func TestTheWorkspaceRootIsNeverInATempDirectory(t *testing.T) {
	root := defaultWorkspaceRoot()
	if root == "" {
		t.Fatal("no workspace root")
	}
	if underTempDir(root) {
		t.Fatalf("workspace root %q is inside the temp directory; it will not exist after a build", root)
	}
	if !filepath.IsAbs(root) {
		t.Errorf("workspace root %q is not absolute", root)
	}
	// The live config must not be holding one either, whatever wrote it.
	if path := os.Getenv("CERAA_CONFIG"); path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			var stored struct {
				WorkspaceRoot string `json:"workspace_root"`
			}
			if json.Unmarshal(raw, &stored) == nil && stored.WorkspaceRoot != "" {
				if underTempDir(stored.WorkspaceRoot) {
					t.Errorf("the stored workspace_root %q is inside the temp directory", stored.WorkspaceRoot)
				}
			}
		}
	}
}

// A real install is never inside the temp directory, and neither is a legitimate
// working directory, so the fallback chain must land on one of those.
func TestUnderTempDir(t *testing.T) {
	temp := filepath.Clean(os.TempDir())
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/workspace/ceraa/workspace", false},
		{"/workspace/ceraa", false},
		{"/", false},
		{"", false},
		{temp, true},
		{filepath.Join(temp, "go-build123/b001"), true},
		{temp + "/", true},
		// A sibling whose name merely starts the same is not inside it.
		{filepath.Join(filepath.Dir(temp), filepath.Base(temp)+"-other"), false},
		{filepath.Join(filepath.Dir(temp), "workspace"), false},
	} {
		if got := underTempDir(tc.path); got != tc.want {
			t.Errorf("underTempDir(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// Where the workspace lands when nobody configured one, because that decides
// whether a fresh install works at all.
//
// The "bin" special case is what makes the common install layouts work, and it
// is also a trap: /opt/ceraa/bin/ceraa correctly yields /opt/ceraa/workspace,
// but /usr/local/bin/ceraa yields /usr/local/workspace, which a non-root user
// cannot create and is a poor place for a project regardless. Ceraa mkdirs the
// workspace at startup, so the failure is a bot that will not start.
func TestTheDerivedWorkspaceRootIsUsableForCommonInstallLayouts(t *testing.T) {
	derive := func(executable string) string {
		directory := filepath.Dir(executable)
		if filepath.Base(directory) == "bin" {
			directory = filepath.Dir(directory)
		}
		return filepath.Join(directory, "workspace")
	}
	for _, tc := range []struct{ executable, want string }{
		// A versioned install directory: the useful case the special case exists for.
		{"/opt/ceraa/bin/ceraa", "/opt/ceraa/workspace"},
		{"/opt/ceraa/ceraa", "/opt/ceraa/workspace"},
		{"/home/dev/myagent/ceraa", "/home/dev/myagent/workspace"},
		{"/home/dev/myagent/bin/ceraa", "/home/dev/myagent/workspace"},
	} {
		if got := derive(tc.executable); got != tc.want {
			t.Errorf("%s -> %s, want %s", tc.executable, got, tc.want)
		}
	}

	// Documented, because it is the layout a systemd unit usually reaches for.
	// CERAA_WORKSPACE_ROOT is the answer; the default is merely survivable when
	// the process is root.
	if got := derive("/usr/local/bin/ceraa"); got != "/usr/local/workspace" {
		t.Fatalf("/usr/local/bin now derives %s; if this ever improves, the deployment "+
			"note in REQUIREMENT.md should say so rather than relying on the env var alone", got)
	}
	if got := derive("/root/ceraa"); got != "/root/workspace" {
		t.Fatalf("/root/ceraa now derives %s", got)
	}
}

// Only CERAA_* is a supported prefix, and a bare legacy override broke that in a
// way that reached the wrong provider.
//
// POOLSIDE_API_URL was the last remnant of a scheme that resolved a fixed set of
// provider keys. It set cfg.LLM.URL unconditionally, which is the URL of the
// built-in "default" profile, so setting it silently redefined what "default"
// pointed at regardless of which provider was selected - and it was not
// CERAA_-prefixed at all.
//
// Poolside itself is unaffected and still fully supported: it has an onboarding
// button and its URL is seeded in the bridge. Only this competing second way to
// point at it is gone.
func TestALegacyUnprefixedOverrideCannotRedirectTheDefaultProvider(t *testing.T) {
	t.Setenv("POOLSIDE_API_URL", "https://inference.poolside.ai/v1")

	cfg := DefaultConfig()
	cfg.LLM.URL = ""
	applyEnv(cfg)

	if cfg.LLM.URL != "" {
		t.Errorf("POOLSIDE_API_URL set the default provider URL to %q; only CERAA_* may overlay config", cfg.LLM.URL)
	}
}

// The supported variables still work, which is the half of that change that has
// to keep working.
func TestTheDocumentedEnvSeedStillApplies(t *testing.T) {
	t.Setenv("CERAA_API_URL", "https://gateway.example/v1")
	t.Setenv("CERAA_MODEL", "some-model")
	t.Setenv("CERAA_API_KEY", "k")

	cfg := DefaultConfig()
	cfg.LLM.URL = ""
	cfg.LLM.Model = ""
	cfg.LLM.APIKey = ""
	applyEnv(cfg)

	if cfg.LLM.URL != "https://gateway.example/v1" || cfg.LLM.Model != "some-model" || cfg.LLM.APIKey != "k" {
		t.Errorf("the documented env seed did not apply: url=%q model=%q key=%q",
			cfg.LLM.URL, cfg.LLM.Model, cfg.LLM.APIKey)
	}
}
