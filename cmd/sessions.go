package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ceraa/harness/sessioncontrol"
	"ceraa/harness/sessionstore"
	"ceraa/internal/config"

	"github.com/spf13/cobra"
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions [session-id]",
	Short: "Inspect saved sessions and runs without changing them",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var sessionID string
		if len(args) == 1 {
			sessionID = args[0]
		}
		return runSessions(cmd.OutOrStdout(), sessionID)
	},
}

// runSessions uses the control facade so the CLI reports the same run state as
// other control surfaces without creating a second view of session data.
func runSessions(out io.Writer, sessionID string) error {
	if sessionID != "" {
		if filepath.Base(sessionID) != sessionID || sessionID == "." || sessionID == ".." {
			return fmt.Errorf("invalid session id %q", sessionID)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfg.SessionFile); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("session store %q does not exist", cfg.SessionFile)
		}
		return fmt.Errorf("inspect session store %q: %w", cfg.SessionFile, err)
	}
	store, err := sessionstore.New(cfg.SessionFile)
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	control, err := sessioncontrol.New(sessioncontrol.Dependencies{Store: store})
	if err != nil {
		return fmt.Errorf("open session control: %w", err)
	}

	ids := []string{sessionID}
	if sessionID == "" {
		ids, err = store.List()
		if err != nil {
			return fmt.Errorf("list sessions: %w", err)
		}
		sort.Strings(ids)
	}
	if len(ids) == 0 {
		fmt.Fprintln(out, "No saved sessions.")
		return nil
	}
	for _, id := range ids {
		status, statusErr := control.Status(context.Background(), sessioncontrol.StatusRequest{SessionID: sessioncontrol.SessionID(id)})
		if statusErr != nil {
			return fmt.Errorf("inspect session %q: %w", id, statusErr)
		}
		printSessionStatus(out, status)
	}
	return nil
}

func printSessionStatus(out io.Writer, status sessioncontrol.SessionStatus) {
	title := singleLine(status.Title)
	if title == "" {
		title = "Untitled session"
	}
	fmt.Fprintf(out, "Session: %s\nStatus: %s\nTitle: %s\n", singleLine(string(status.SessionID)), singleLine(string(status.Status)), title)
	if len(status.Runs) == 0 {
		fmt.Fprintln(out, "Runs: none")
		return
	}
	for _, run := range status.Runs {
		fmt.Fprintf(out, "Run: %s  Status: %s  Steps: %d  Requests: %d\n", singleLine(string(run.ID)), singleLine(string(run.Status)), run.Step, run.Requests)
	}
}

// singleLine keeps model- or file-provided values from breaking the record layout.
func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
