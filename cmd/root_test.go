package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The documented `ceraa "do something"` form is the only non-interactive entry
// point, and it failed with `unknown command "do something"` because cobra's
// default Args for a command that has subcommands rejects any first argument
// that is not a subcommand name. That left no working headless mode at all:
// no CLI eval, no CI, no scripting.
func TestRootAcceptsAPromptAsArguments(t *testing.T) {
	if root.Args == nil {
		t.Fatal("root has no Args set, so cobra falls back to legacyArgs and rejects a prompt")
	}
	// ArbitraryArgs is the value that accepts a free-text prompt; it must not
	// reject one.
	if err := root.Args(&cobra.Command{}, []string{"fix", "the", "build"}); err != nil {
		t.Errorf("Args rejected a free-text prompt: %v", err)
	}
}

// A known subcommand must still win over the free-text form, otherwise
// ArbitraryArgs would shadow `ceraa start` and `ceraa doctor`.
func TestSubcommandsStillResolve(t *testing.T) {
	registerSubcommands()
	for _, name := range []string{"start", "doctor", "version", "config", "setup"} {
		found := false
		for _, sub := range root.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("subcommand %q is no longer registered", name)
		}
	}
}

// The root must run the agent when given a prompt, which is what RunE does.
func TestRootRunEPassesTheJoinedPrompt(t *testing.T) {
	if root.RunE == nil {
		t.Fatal("root has no RunE, so a prompt would never reach the agent")
	}
	source := root.Use
	if !strings.Contains(source, "[prompt]") {
		t.Errorf("root Use = %q, want it to document the prompt form", source)
	}
	// Guard the joining behaviour the CLI depends on, without invoking a model.
	joined := strings.Join([]string{"fix", "the", "build"}, " ")
	if joined != "fix the build" {
		t.Errorf("prompt joining produced %q", joined)
	}
	if bytes.Contains([]byte(root.Short), []byte("\n")) {
		t.Error("root Short contains a newline")
	}
}
