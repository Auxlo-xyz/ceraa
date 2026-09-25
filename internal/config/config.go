package config

import (
	"ceraa/internal/envs"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"ceraa/internal/paths"
)

// env reads a Ceraa setting from the environment; see internal/envs for the
// single definition of the prefix.
func env(name string) string {
	return envs.Get(name)
}

// Config holds all Ceraa settings.
type Config struct {
	LLM                      LLMProvider                `json:"llm"`
	ProviderProfiles         map[string]ProviderProfile `json:"provider_profiles,omitempty"`
	ActiveProvider           string                     `json:"active_provider,omitempty"`
	Subscriptions            Subscriptions              `json:"subscriptions,omitempty"`
	Telegram                 TelegramConfig             `json:"telegram,omitempty"`
	SessionFile              string                     `json:"session_file,omitempty"`
	WorkspaceRoot            string                     `json:"workspace_root,omitempty"`
	ToolOutputDir            string                     `json:"tool_output_dir,omitempty"`
	MaxToolResultChars       int                        `json:"max_tool_result_chars,omitempty"`
	MaxConcurrentTools       int                        `json:"max_concurrent_tools,omitempty"`
	ModelCatalogURL          string                     `json:"model_catalog_url,omitempty"`
	ModelCatalogCache        string                     `json:"model_catalog_cache,omitempty"`
	ModelCatalogRefreshHours int                        `json:"model_catalog_refresh_hours,omitempty"`
	TitleAPIURL              string                     `json:"title_api_url,omitempty"`
	TitleModel               string                     `json:"title_model,omitempty"`
	MemoryFile               string                     `json:"memory_file,omitempty"`
	SkillFile                string                     `json:"skill_file,omitempty"`
	EvaluationFile           string                     `json:"evaluation_file,omitempty"`
	SchedulerFile            string                     `json:"scheduler_file,omitempty"`
}

// LLMProvider holds LLM API settings.
type LLMProvider struct {
	URL             string `json:"url"`
	Model           string `json:"model"`
	APIKey          string `json:"api_key,omitempty"`
	ContextLimit    int    `json:"context_limit,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	PromptCacheMode string `json:"prompt_cache_mode,omitempty"`
	PromptCacheTTL  string `json:"prompt_cache_ttl,omitempty"`
	Subscription    string `json:"subscription,omitempty"`
	HermesProvider  string `json:"hermes_provider,omitempty"`
}

type ProviderProfile struct {
	URL             string `json:"url"`
	APIKey          string `json:"api_key,omitempty"`
	Model           string `json:"model"`
	ContextLimit    int    `json:"context_limit,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	PromptCacheMode string `json:"prompt_cache_mode,omitempty"`
	PromptCacheTTL  string `json:"prompt_cache_ttl,omitempty"`
	Subscription    string `json:"subscription,omitempty"`
	HermesProvider  string `json:"hermes_provider,omitempty"`
}

// Subscriptions holds provider subscription settings.
type Subscriptions struct {
	Providers map[string]SubProvider `json:"providers"`
}

// SubProvider holds subscription-specific settings.
type SubProvider struct {
	Kind    string            `json:"kind"`
	APIKey  string            `json:"api_key,omitempty"`
	CLIPath string            `json:"cli_path,omitempty"`
	AuthDir string            `json:"auth_dir,omitempty"`
	Extra   map[string]string `json:"extra,omitempty"`
}

type TelegramConfig struct {
	BotToken      string `json:"bot_token,omitempty"`
	AllowedUserID int64  `json:"allowed_user_id,omitempty"`
}

func defaultWorkspaceRoot() string {
	if executable, err := os.Executable(); err == nil {
		if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
			executable = resolved
		}
		directory := filepath.Dir(executable)
		if filepath.Base(directory) == "bin" {
			directory = filepath.Dir(directory)
		}
		return filepath.Join(directory, "workspace")
	}
	if workingDirectory, err := os.Getwd(); err == nil {
		return filepath.Join(workingDirectory, "workspace")
	}
	return paths.DefaultWorkspaceRoot()
}

// DefaultConfig returns the base defaults before env/file overrides.
func DefaultConfig() *Config {
	base := paths.DataDir()
	return &Config{
		LLM:                      LLMProvider{ContextLimit: 236000, MaxOutputTokens: 8192, PromptCacheMode: "auto"},
		ProviderProfiles:         map[string]ProviderProfile{},
		SessionFile:              "sessions",
		WorkspaceRoot:            defaultWorkspaceRoot(),
		MaxToolResultChars:       12000,
		MaxConcurrentTools:       8,
		ModelCatalogURL:          "https://openrouter.ai/api/v1/models",
		ModelCatalogRefreshHours: 24,
		TitleAPIURL:              "https://generativelanguage.googleapis.com/v1beta",
		TitleModel:               "gemini-3.5-flash-lite",
		MemoryFile:               filepath.Join(base, "memory"),
		SkillFile:                filepath.Join(base, "skills"),
		EvaluationFile:           filepath.Join(base, "evaluations.json"),
		SchedulerFile:            filepath.Join(base, "scheduler.json"),
	}
}

func (c *Config) WorkspaceRootPath() string {
	root := c.WorkspaceRoot
	if root == "" {
		root = defaultWorkspaceRoot()
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return root
	}
	return abs
}

func (c *Config) ActiveLLM() LLMProvider {
	id := c.ActiveProvider
	if id == "" {
		id = "default"
	}
	profile, ok := c.ProviderProfile(id)
	if !ok {
		return c.LLM
	}
	result := c.LLM
	if profile.URL != "" {
		result.URL = profile.URL
	}
	if profile.APIKey != "" {
		result.APIKey = profile.APIKey
	}
	if profile.Model != "" {
		result.Model = profile.Model
	}
	if profile.ContextLimit > 0 {
		result.ContextLimit = profile.ContextLimit
	}
	if profile.MaxOutputTokens > 0 {
		result.MaxOutputTokens = profile.MaxOutputTokens
	}
	if profile.PromptCacheMode != "" {
		result.PromptCacheMode = profile.PromptCacheMode
	}
	if profile.PromptCacheTTL != "" {
		result.PromptCacheTTL = profile.PromptCacheTTL
	}
	if profile.Subscription != "" {
		result.Subscription = profile.Subscription
	}
	if profile.HermesProvider != "" {
		result.HermesProvider = profile.HermesProvider
	}
	return result
}

func (c *Config) ProviderProfile(id string) (ProviderProfile, bool) {
	if profile, ok := c.ProviderProfiles[id]; ok {
		return profile, true
	}
	if id == "default" {
		return ProviderProfile{URL: c.LLM.URL, APIKey: c.LLM.APIKey, Model: c.LLM.Model, ContextLimit: c.LLM.ContextLimit, MaxOutputTokens: c.LLM.MaxOutputTokens, PromptCacheMode: c.LLM.PromptCacheMode, PromptCacheTTL: c.LLM.PromptCacheTTL, Subscription: c.LLM.Subscription, HermesProvider: c.LLM.HermesProvider}, true
	}
	return ProviderProfile{}, false
}

func (c *Config) ProviderIDs() []string {
	ids := make([]string, 0, len(c.ProviderProfiles)+1)
	ids = append(ids, "default")
	for id := range c.ProviderProfiles {
		if id != "default" {
			ids = append(ids, id)
		}
	}
	return ids
}

// Load loads config from config file + env override + defaults.
func Load() (*Config, error) {
	cfg := DefaultConfig()

	// Try config file
	if err := loadFile(cfg); err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}

	// Env overrides
	if v := env("PROVIDER"); v != "" {
		cfg.ActiveProvider = v
	}
	if v := env("API_URL"); v != "" {
		cfg.LLM.URL = v
	}
	if v := env("MODEL"); v != "" {
		cfg.LLM.Model = v
	}
	if v := env("API_KEY"); v != "" {
		cfg.LLM.APIKey = v
	}
	if v := env("CONTEXT_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.LLM.ContextLimit = n
		}
	}
	if v := env("MAX_OUTPUT_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.LLM.MaxOutputTokens = n
		}
	}
	if v := env("PROMPT_CACHE_MODE"); v != "" {
		cfg.LLM.PromptCacheMode = v
	}
	if v := env("PROMPT_CACHE_TTL"); v != "" {
		cfg.LLM.PromptCacheTTL = v
	}
	if v := env("SUBSCRIPTION"); v != "" {
		cfg.LLM.Subscription = v
	}
	if v := env("HERMES_PROVIDER"); v != "" {
		cfg.LLM.HermesProvider = v
	}
	if v := env("SESSION_FILE"); v != "" {
		cfg.SessionFile = v
	}
	if v := env("WORKSPACE_ROOT"); v != "" {
		cfg.WorkspaceRoot = v
	}
	if v := env("TOOL_OUTPUT_DIR"); v != "" {
		cfg.ToolOutputDir = v
	}
	if v := env("MAX_TOOL_RESULT_CHARS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxToolResultChars = n
		}
	}
	if v := env("MAX_CONCURRENT_TOOLS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxConcurrentTools = n
		}
	}
	if v := env("MODEL_CATALOG_URL"); v != "" {
		cfg.ModelCatalogURL = v
	}
	if v := env("MODEL_CATALOG_CACHE"); v != "" {
		cfg.ModelCatalogCache = v
	}
	if v := env("MODEL_CATALOG_REFRESH_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.ModelCatalogRefreshHours = n
		}
	}
	if v := env("TITLE_API_URL"); v != "" {
		cfg.TitleAPIURL = v
	}
	if v := env("TITLE_MODEL"); v != "" {
		cfg.TitleModel = v
	}
	if v := env("MEMORY_FILE"); v != "" {
		cfg.MemoryFile = v
	}
	if v := env("SKILL_FILE"); v != "" {
		cfg.SkillFile = v
	}
	if v := env("EVALUATION_FILE"); v != "" {
		cfg.EvaluationFile = v
	}
	if v := env("SCHEDULER_FILE"); v != "" {
		cfg.SchedulerFile = v
	}

	// Poolside provider env override
	if v := os.Getenv("POOLSIDE_API_URL"); v != "" {
		cfg.LLM.URL = v
	}

	return cfg, nil
}

func loadFile(cfg *Config) error {
	paths := candidatePaths()
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := json.Unmarshal(data, cfg); err != nil {
			return fmt.Errorf("parse %s: %w", p, err)
		}
		return nil
	}
	return nil
}

func Save(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	path := filepath.Join(paths.DataDir(), "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create config temp file: %w", err)
	}
	tmpPath := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	remove = false
	return nil
}

// candidatePaths lists config.json locations in precedence order. The legacy
// locations are appended so an install that has not been moved still finds its
// config, and a Ceraa config always takes precedence over a pre-rename one.
func candidatePaths() []string {
	return paths.ConfigCandidates()
}
