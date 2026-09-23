package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config holds all soraa-agent settings.
type Config struct {
	Provider    ProviderConfig `json:"provider"`
	Telegram    TelegramConfig `json:"telegram,omitempty"`
	MaxSteps    int            `json:"max_steps,omitempty"`
	SessionFile string         `json:"session_file,omitempty"`
}

type ProviderConfig struct {
	URL    string `json:"url"`
	Model  string `json:"model"`
	APIKey string `json:"api_key,omitempty"`
}

type TelegramConfig struct {
	BotToken      string `json:"bot_token,omitempty"`
	AllowedUserID int64  `json:"allowed_user_id,omitempty"`
}

// DefaultConfig returns the base defaults before env/file overrides.
func DefaultConfig() *Config {
	return &Config{
		Provider: ProviderConfig{
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
	if v := os.Getenv("SORAA_API_URL"); v != "" {
		cfg.Provider.URL = v
	}
	if v := os.Getenv("SORAA_MODEL"); v != "" {
		cfg.Provider.Model = v
	}
	if v := os.Getenv("SORAA_API_KEY"); v != "" {
		cfg.Provider.APIKey = v
	}
	if v := os.Getenv("TELEGRAM_BOT_TOKEN"); v != "" {
		cfg.Telegram.BotToken = v
	}
	if v := os.Getenv("TELEGRAM_ALLOWED_USER_ID"); v != "" {
		var id int64
		fmt.Sscanf(v, "%d", &id)
		cfg.Telegram.AllowedUserID = id
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
		filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "soraa-agent", "config.json"),
		filepath.Join(home, ".config", "soraa-agent", "config.json"),
		"./soraa-agent.json",
	}
}
