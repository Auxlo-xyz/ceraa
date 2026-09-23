package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config holds all sooraa settings.
type Config struct {
	LLM           LLMProvider    `json:"llm"`
	Subscriptions Subscriptions  `json:"subscriptions,omitempty"`
	Telegram      TelegramConfig `json:"telegram,omitempty"`
	MaxSteps      int            `json:"max_steps,omitempty"`
	SessionFile   string         `json:"session_file,omitempty"`
}

// LLMProvider holds LLM API settings.
type LLMProvider struct {
	URL    string `json:"url"`
	Model  string `json:"model"`
	APIKey string `json:"api_key,omitempty"`
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

// DefaultConfig returns the base defaults before env/file overrides.
func DefaultConfig() *Config {
	return &Config{
		LLM: LLMProvider{
			URL:   "https://api.atria-asi.ai/v1",
			Model: "atria-dawn-preview",
		},
		MaxSteps:    10,
		SessionFile: "sessions",
	}
}

// Load loads config from config file + env override + defaults.
func Load() (*Config, error) {
	cfg := DefaultConfig()

	// Try config file
	if err := loadFile(cfg); err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}

	// Env overrides
	if v := os.Getenv("SOORAA_API_URL"); v != "" {
		cfg.LLM.URL = v
	}
	if v := os.Getenv("Sooraa_MODEL"); v != "" {
		cfg.LLM.Model = v
	}
	if v := os.Getenv("SOORAA_API_KEY"); v != "" {
		cfg.LLM.APIKey = v
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

func candidatePaths() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "sooraa", "config.json"),
		filepath.Join(home, ".config", "sooraa", "config.json"),
		"./sooraa.json",
	}
}
