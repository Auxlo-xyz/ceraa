package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The README documents the service lifecycle, and a document that names commands
// nobody added is worse than one that names none: an operator reads the table,
// runs the command, and is told it does not exist.
//
// This is the same failure already fixed once in the README, where the run path
// pointed at a systemd unit on a host that has no systemd. That was found by
// reading, which is exactly what this prevents needing.
func TestTheReadmeDocumentsEveryServiceLifecycleCommand(t *testing.T) {
	dir := repoRoot(t)
	readme := readFile(t, filepath.Join(dir, "README.md"))

	// The commands an operator needs to run and stop a daemon, plus the one that
	// tells them when something is wrong.
	lifecycle := []string{"start", "stop", "restart", "doctor", "version"}

	registered := map[string]bool{}
	for _, sub := range registeredCommands() {
		registered[sub.Name()] = true
	}

	// The table is the contract, and the check is deliberately narrow. A loose
	// "does the word appear anywhere" search passes for the wrong reason:
	// `ceraa stop` also appears in the supervisor section as the command that
	// script wraps, so deleting the table row entirely still satisfies a
	// substring check. Each lifecycle command therefore needs its own row.
	rows := tableRows(readme)
	for _, name := range lifecycle {
		if !registered[name] {
			t.Fatalf("the README documents `ceraa %s` but the command does not exist", name)
		}
		if !rows[name] {
			t.Errorf("`ceraa %s` exists but has no row in the README lifecycle table", name)
		}
	}
}

// tableRows returns the command named by the first cell of each markdown table
// row that names one.
func tableRows(page string) map[string]bool {
	rows := map[string]bool{}
	for _, line := range strings.Split(page, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cells := strings.Split(trimmed, "|")
		if len(cells) < 3 {
			continue
		}
		first := strings.Trim(strings.TrimSpace(cells[1]), "`* ")
		if !strings.HasPrefix(first, "ceraa ") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(first, "ceraa "), " ")
		if name != "" {
			rows[name] = true
		}
	}
	return rows
}

// registeredCommands returns the subcommands the binary actually offers.
//
// registerSubcommands is what attaches them, and it is normally called from
// Execute. Calling it here is the point: reading root.Commands() without it
// returns an empty list, which is how a doc test would pass against a binary that
// has no subcommands at all.
func registeredCommands() []*cobra.Command {
	registerSubcommands()
	return root.Commands()
}

// The supervisor is how the daemon is actually kept alive, so a README that
// documents the commands but not the watcher sends an operator to `ceraa start`
// in the foreground and leaves them with a bot that dies silently.
func TestTheReadmeDocumentsTheSupervisor(t *testing.T) {
	page := readFile(t, filepath.Join(repoRoot(t), "README.md"))
	for _, want := range []string{"ceraa-supervisor.sh", "watch"} {
		if !strings.Contains(page, want) {
			t.Errorf("the README never mentions %q, so the only way to keep the daemon alive is undocumented", want)
		}
	}
}

// The systemd unit is a sample, and it referenced three paths that do not exist
// on the layout the README documents. A unit that cannot start is worse than one
// that is absent, because it looks authoritative.
func TestTheSystemdUnitPathsArePlausible(t *testing.T) {
	unit := readFile(t, filepath.Join(repoRoot(t), "ceraa.service"))

	// The unit must not send logs to /tmp. That was the bug: /tmp is wiped on a
	// recycled host, so the service log disappears with the crash it explains.
	if strings.Contains(unit, "/tmp/") {
		t.Error("ceraa.service writes to /tmp, which does not survive a host recycle")
	}
	if !strings.Contains(unit, "runtime/bin/ceraa") {
		t.Errorf("ceraa.service does not point at the deployed binary path the README documents:\n%s", unit)
	}
	if !strings.Contains(unit, "EnvironmentFile=") {
		t.Error("ceraa.service loads no EnvironmentFile, so it has no way to receive the bot token")
	}
}

// repoRoot walks up from the test's directory to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find go.mod above the test")
	return ""
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
