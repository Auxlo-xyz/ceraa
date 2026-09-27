package cmd

import (
	"ceraa/internal/agent"
	"ceraa/internal/config"
	"ceraa/internal/paths"
	"ceraa/internal/procfile"
	"ceraa/internal/telegram"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var root = &cobra.Command{
	Use:   "ceraa [prompt]",
	Short: "Ceraa - coding harness",
	// Without this, cobra's default for a command that has subcommands rejects
	// any first argument that is not a subcommand name, so the documented
	// `ceraa "do something"` failed with `unknown command "do something"`. A known
	// subcommand still wins, so `ceraa start` and `ceraa doctor` are unaffected.
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		// A near miss of a real subcommand is a typo, not a prompt. Without this,
		// `ceraa stopp` was sent to the model as a prompt and answered, spending a
		// real inference call on a one-word slip.
		if suggestion, ok := nearMiss(args[0], subcommandNames()); ok {
			return errNearMiss(args[0], suggestion)
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

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop a running Ceraa service",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStop()
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

// stopGrace is how long a stop waits for a SIGTERM to be honoured before it
// escalates. Long enough for the poll loop to notice a cancelled context and run
// its deferred teardown, short enough that a wedged daemon is not a long wait.
const stopGrace = 8 * time.Second

func runStop() error {
	path := procfile.Path(paths.DataDir())
	rec, err := procfile.Read(path)
	if os.IsNotExist(err) {
		fmt.Println("Ceraa is not running (no pid file).")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read pid file: %w", err)
	}
	// Clear a record left by a daemon that died without running its cleanup, so
	// the next start is not blocked by a slot nobody holds.
	matches, matchErr := procfile.Matches(rec)
	if matchErr != nil || !matches {
		if rmErr := procfile.Remove(path); rmErr != nil {
			return rmErr
		}
		fmt.Printf("Ceraa is not running; cleared stale pid file for %d.\n", rec.PID)
		return nil
	}
	outcome, termErr := procfile.Terminate(rec, stopGrace)
	switch outcome {
	case procfile.AlreadyGone:
		procfile.Remove(path)
		fmt.Println("Ceraa exited before the signal landed.")
	case procfile.StoppedCleanly:
		procfile.Remove(path)
		fmt.Printf("Ceraa stopped (pid %d, SIGTERM).\n", rec.PID)
	case procfile.StoppedKilled:
		procfile.Remove(path)
		fmt.Printf("Ceraa did not exit within %s; killed (pid %d).\n", stopGrace, rec.PID)
	}
	return termErr
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Show resolved configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		// Report the provider that would actually be used, not the base block.
		// Reading cfg.LLM directly reported an empty provider for an install whose
		// provider had been chosen from the Telegram menu, so this command said
		// "no provider" while a session was demonstrably running on one.
		active := cfg.ActiveLLM()
		if active.URL == "" {
			fmt.Println("Provider URL: (none configured)")
		} else {
			fmt.Printf("Provider URL: %s\n", active.URL)
		}
		fmt.Printf("Model:        %s\n", active.Model)
		fmt.Printf("API Key:      %s\n", maskKey(active.APIKey))
		if id := cfg.ActiveProvider; id != "" {
			fmt.Printf("Profile:      %s\n", id)
		}
		if state, err := config.LoadRuntimeState(); err == nil && state.Provider != "" {
			fmt.Printf("Runtime:      %s (set from the Telegram menu)\n", state.Provider)
		}
		fmt.Printf("Telegram:     %s\n", maskToken(cfg.Telegram.BotToken))
		fmt.Printf("User ID:      %d\n", cfg.Telegram.AllowedUserID)
		fmt.Printf("Session File: %s\n", cfg.SessionFile)
		fmt.Printf("Workspace:    %s\n", cfg.WorkspaceRootPath())
		return nil
	},
}

var configInitCmd = &cobra.Command{
	Use:   "config-init",
	Short: "Write a sample config file, refusing to overwrite an existing one",
	RunE: func(cmd *cobra.Command, args []string) error {
		// No workspace_root key. The sample used to carry the literal string
		// "/absolute/path/to/ceraa/workspace", which is a placeholder for a human to
		// replace, not a path: it was written as a real setting and then read back
		// as one, so the install pointed its workspace at a directory that does not
		// exist. Omitting the key leaves the resolved default in place, which is
		// what a fresh install wants anyway.
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
  "session_file": "sessions"
}
`
		dir := paths.DataDir()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(dir, "config.json")
		// Refuse to clobber. A sample file is only useful before there is a real
		// one, and overwriting a live config destroys the provider, the workspace
		// and the session settings with no confirmation and no backup.
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("refusing to overwrite the existing config at %s; edit it, or move it aside first", path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("check for an existing config: %w", err)
		}
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

// registerSubcommands attaches the built-in subcommands to root.
//
// It is separate from Execute so the wiring is testable: registering inside
// Execute meant the subcommands did not exist until the binary ran, so nothing
// could assert that they still resolve.
func registerSubcommands() {
	attach := func(command *cobra.Command) {
		root.AddCommand(command)
		recordSubcommand(command)
	}
	attach(verCmd)
	attach(startCmd)
	attach(stopCmd)
	attach(setupCmd)
	attach(doctorCmd)
	attach(configCmd)
	attach(configInitCmd)
}

func Execute() {
	registerSubcommands()
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
