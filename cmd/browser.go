package cmd

// browser.go exposes the shared browser as a shell command.
//
// It is a command rather than a tool because a schema for each action cost
// 3,051 bytes of the cached catalogue and taught one action per call. The model
// already knows how to run a shell command, and one command can carry what
// used to be several round trips.

import (
	"ceraa/harness/tool"

	"github.com/spf13/cobra"
)

// BrowserCommand returns the `ceraa browser` command.
//
// A constructor rather than a package-level variable, for the same reason
// ReportCommand is: the command carries no flags of its own, but a test that
// built one should not leave process-wide state behind.
func BrowserCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "browser <action> [arguments]",
		Short: "Drive the shared headless browser from the shell",
		Long: "Drive the shared headless browser from the shell.\n" +
			"The page persists between commands, and `snapshot` numbers the controls\n" +
			"(@1, @2, ...) so `click @3` and `type @3` do not depend on a CSS class.\n" +
			"Actions: open, snapshot, text, html, click, type, eval, shot, status, close.",
		Args: cobra.ArbitraryArgs,
		// The action's own flags belong to the action, not to cobra: `ceraa
		// browser open --file x` must reach open rather than be rejected as an
		// unknown flag of the parent command.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return tool.RunBrowserCLI(cmd.Context(), cmd.OutOrStdout(), args)
		},
	}
}
