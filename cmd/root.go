package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"soraa-agent/internal/agent"
	"soraa-agent/internal/config"
)

var root = &cobra.Command{
	Use:   "soraa-agent [prompt]",
	Short: "Soraa Agent - coding harness",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		return agent.Run(strings.Join(args, " "))
	},
}

var verCmd = &cobra.Command{Use: "version", Run: func(cmd *cobra.Command, args []string) { fmt.Println("soraa-agent 0.1.0") }}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Show resolved configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		fmt.Printf("Provider URL: %s\n", cfg.LLM.URL)
		fmt.Printf("Model:        %s\n", cfg.LLM.Model)
		fmt.Printf("API Key:      %s\n", maskKey(cfg.LLM.APIKey))
		fmt.Printf("Telegram:     %s\n", maskToken(cfg.Telegram.BotToken))
		fmt.Printf("User ID:      %d\n", cfg.Telegram.AllowedUserID)
		fmt.Printf("Max Steps:    %d\n", cfg.MaxSteps)
		fmt.Printf("Session File: %s\n", cfg.SessionFile)
		return nil
	},
}

var configInitCmd = &cobra.Command{
	Use:   "config-init",
	Short: "Write a sample config file to ~/.config/soraa-agent/config.json",
	RunE: func(cmd *cobra.Command, args []string) error {
		sample := `{
  "llm": {
    "url": "https://api.atria-asi.ai/v1",
    "model": "atria-dawn-preview",
    "api_key": "atr_..."
  },
  "subscriptions": {
    "providers": {
      "opencode": { "kind": "opencode" },
      "hermes": { "kind": "hermes" },
      "codex": { "kind": "codex" }
    }
  },
  "telegram": {
    "bot_token": "123:ABC...",
    "allowed_user_id": 5967460976
  },
  "max_steps": 10,
  "session_file": "sessions"
}
`
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir := filepath.Join(home, ".config", "soraa-agent")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
			return err
		}
		fmt.Printf("Config written to %s\n", path)
		return nil
	},
}

func maskKey(s string) string {
	if len(s) <= 4 {
		return "***"
	}
	return s[:4] + "..."
}

func maskToken(s string) string {
	if len(s) <= 4 {
		return "not set"
	}
	return "set"
}

func Execute() {
	root.AddCommand(verCmd)
	root.AddCommand(configCmd)
	root.AddCommand(configInitCmd)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
