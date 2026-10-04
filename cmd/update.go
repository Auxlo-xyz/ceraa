package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"ceraa/internal/build"
	"ceraa/internal/paths"
	"ceraa/internal/procfile"
	"ceraa/internal/selfupdate"

	"github.com/spf13/cobra"
)

var (
	updateCheckFlag   bool
	updateVersionFlag string
)

// updateCmd replaces the running binary with a published release.
//
// Self-update exists because the alternative is worse: an installed copy has no
// source and no build toolchain, so without this the only way to move to a new
// release is to remember that one happened and re-run install.sh by hand. For a
// bridge that runs unattended that means it never moves at all.
var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update Ceraa to a newer release",
	Long: `Update replaces the running binary with a published release.

The download is checked against the release's own SHA256SUMS, and the new file
is run once to confirm it starts and reports the version the tag names before
anything is renamed over your copy. A candidate that fails either check costs
you nothing.

A service that is already running keeps executing the version it started with,
because the operating system still holds the old file open. Stop it afterwards
to switch; update prints the pid when there is one.

Use --check to ask without changing anything.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runUpdate(cmd)
	},
}

func init() {
	updateCmd.Flags().BoolVar(&updateCheckFlag, "check", false, "report whether an update exists, changing nothing")
	updateCmd.Flags().StringVar(&updateVersionFlag, "version", "", "update to this tag instead of the latest, e.g. v0.5.0")
}

func runUpdate(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	ctx := context.Background()

	// --check answers and stops. It exits zero either way, because "you are
	// current" is a successful check, not a failure; the line it prints is what
	// a script should branch on.
	if updateCheckFlag {
		status, err := selfupdate.Check(ctx)
		if err != nil {
			return fmt.Errorf("check for a newer release: %w", err)
		}
		fmt.Fprintln(out, status)
		return nil
	}

	// A pinned tag skips the lookup, so asking for v0.4.6 installs v0.4.6 even
	// when something newer exists.
	if updateVersionFlag != "" {
		return applyUpdate(out, ctx, updateVersionFlag, build.Version)
	}

	status, err := selfupdate.Check(ctx)
	if err != nil {
		return fmt.Errorf("check for a newer release: %w", err)
	}
	if !status.Available {
		fmt.Fprintln(out, status)
		return nil
	}
	return applyUpdate(out, ctx, status.Latest, status.Current)
}

func applyUpdate(out io.Writer, ctx context.Context, tag, from string) error {
	fmt.Fprintf(out, "Fetching %s\n", tag)
	result, err := selfupdate.Apply(ctx, tag)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated %s: %s -> %s\n", result.Binary, from, strings.TrimPrefix(result.Tag, "v"))
	fmt.Fprintf(out, "Verified %q\n", result.Probed)
	if result.Prompt != "" {
		fmt.Fprintf(out, "Refreshed %s\n", result.Prompt)
	}
	if pid, ok := runningServicePID(); ok {
		fmt.Fprintf(out, "\nA Ceraa service is running (pid %d) and still holds the old version open.\nRun `ceraa stop`, then start it again to run %s.\n", pid, strings.TrimPrefix(result.Tag, "v"))
	}
	return nil
}

// runningServicePID reports a bridge started by `ceraa start`, if one is alive.
//
// Read from the pid file rather than a process pattern, for the reason the stop
// command does the same: pgrep would match this very process's command line.
func runningServicePID() (int, bool) {
	rec, err := procfile.Read(procfile.Path(paths.DataDir()))
	if err != nil {
		return 0, false
	}
	matches, err := procfile.Matches(rec)
	if err != nil || !matches {
		return 0, false
	}
	return rec.PID, true
}
