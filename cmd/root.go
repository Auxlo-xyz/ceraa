package cmd

import (
	"ceraa/internal/agent"
	"ceraa/internal/config"
	"ceraa/internal/paths"
	"ceraa/internal/telegram"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var root = &cobra.Command{
	Use:   "ceraa [prompt]",
	Short: "Ceraa - coding harness",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		return agent.Run(strings.Join(args, " "))
	},
}

var verCmd = &cobra.Command{Use: "version", Run: func(cmd *cobra.Command, args []string) { fmt.Println("ceraa 0.1.0") }}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Ceraa Telegram service",
	RunE: func(cmd *cobra.Command, args []string) error {
		return telegram.Run()
	},
}

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure Ceraa interactively",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSetup(os.Stdin, os.Stdout)
	},
}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check configuration, provider, Telegram, workspace, and storage",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDoctor()
	},
}

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
		fmt.Printf("Session File: %s\n", cfg.SessionFile)
		fmt.Printf("Workspace:    %s\n", cfg.WorkspaceRootPath())
		return nil
	},
}

var configInitCmd = &cobra.Command{
	Use:   "config-init",
	Short: "Write a sample config file to ~/.config/ceraa/config.json",
	RunE: func(cmd *cobra.Command, args []string) error {
		sample := `{
  "llm": {
    "url": "",
    "model": "",
    "api_key": ""
  },
  "telegram": {
    "bot_token": "",
    "allowed_user_id": 0
  },
  "session_file": "sessions",
  "workspace_root": "/absolute/path/to/ceraa/workspace"
}
`
		dir := paths.DataDir()
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
	root.AddCommand(startCmd)
	root.AddCommand(setupCmd)
	root.AddCommand(doctorCmd)
	root.AddCommand(configCmd)
	root.AddCommand(configInitCmd)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
