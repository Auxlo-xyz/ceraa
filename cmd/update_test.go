package cmd

import (
	"strings"
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

// What update tells an operator to do next, after it has replaced the file under
// a running daemon.
//
// The obvious advice is "stop it and start it again", and it was what this said.
// It is wrong twice over. It reads like the binary is gone, because renaming
// over a running executable leaves /proc/<pid>/exe showing "(deleted)" - which
// invites a needless stop. And stop/start leaves the bot down long enough for a
// polling supervisor to notice, when `ceraa restart` switches in place and keeps
// the pid.
//
// The reason restart is safe is the thing most likely to be "fixed" away: Go's
// os.Executable strips the " (deleted)" suffix, so ExecSelf resolves to a path
// that now holds the NEW binary. Verified by having a process rename a copy over
// its own path and then stat what os.Executable returned.
func TestUpdateTellsTheOperatorToRestartNotToStop(t *testing.T) {
	notice := runningServiceNotice(4242, "v0.6.5")

	if !strings.Contains(notice, "`ceraa restart`") {
		t.Errorf("the notice does not point at ceraa restart:\n%s", notice)
	}
	// The advice it must NOT give: stopping is the slower, gap-producing path.
	for _, unwanted := range []string{"`ceraa stop`", "ceraa stop, then start", "Stop it afterwards"} {
		if strings.Contains(notice, unwanted) {
			t.Errorf("the notice still tells the operator to stop (found %q):\n%s", unwanted, notice)
		}
	}
	// It has to name the pid, or the operator cannot tell which service it means.
	if !strings.Contains(notice, "4242") {
		t.Errorf("the notice does not name the running pid:\n%s", notice)
	}
	// The tag arrives as "v0.6.5"; the prose should read "0.6.5".
	if strings.Contains(notice, "v0.6.5") {
		t.Errorf("the notice shows the raw tag with its v prefix:\n%s", notice)
	}
	if !strings.Contains(notice, "0.6.5") {
		t.Errorf("the notice does not name the version to switch to:\n%s", notice)
	}
	// The reassurance is the point. An operator who has seen "(deleted)" in lsof
	// needs to be told the path still resolves.
	if !strings.Contains(notice, "deleted") {
		t.Errorf("the notice does not explain the deleted-inode surprise:\n%s", notice)
	}

	// The long help must agree with what the command actually prints, or one of
	// the two will send the operator down the stop path.
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
	if !strings.Contains(update.Long, "Restart it afterwards") {
		t.Errorf("update's long help no longer agrees with the notice it prints:\n%s", update.Long)
	}
}
