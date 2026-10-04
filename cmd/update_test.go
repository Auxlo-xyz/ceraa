package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

// Self-update is only self-update if the operator can find it. The command, its
// two flags and a non-empty description are all load-bearing: an unregistered
// command is unreachable, and a command with no Short renders as a blank row in
// the wrapped help the phone screen depends on.
func TestUpdateIsRegisteredWithItsFlags(t *testing.T) {
	registerSubcommands()

	var update *cobra.Command
	for _, command := range root.Commands() {
		if command.Name() == "update" {
			update = command
		}
	}
	if update == nil {
		t.Fatal("update is not attached to root")
	}
	if update.Short == "" {
		t.Error("update has no short description, so help would show an empty row")
	}
	for _, name := range []string{"check", "version"} {
		if update.Flags().Lookup(name) == nil {
			t.Errorf("--%s is not defined on update", name)
		}
	}
	// Recorded for the near-miss guard like every other subcommand, so a typo of
	// it is reported rather than sent to the model as a prompt.
	found := false
	for _, name := range subcommandNames() {
		if name == "update" {
			found = true
		}
	}
	if !found {
		t.Error("update is not recorded for the near-miss guard")
	}
}
