package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"ceraa/internal/secret"
)

// TestFreebuffCmdOpensTheCredentialStore pins that every freebuff subcommand has
// an initialised store. Without it a browser approval succeeds and the token is
// then dropped at the save step, which is silent and unrecoverable.
func TestFreebuffCmdOpensTheCredentialStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	if freebuffCmd.PersistentPreRun == nil {
		t.Fatal("freebuff command has no PersistentPreRun")
	}
	freebuffCmd.PersistentPreRun(freebuffCmd, nil)
	if err := secret.Set("probe", "value"); err != nil {
		t.Fatalf("store is not usable: %v", err)
	}
	if got, err := secret.Get("probe"); err != nil || got != "value" {
		t.Fatalf("Get = %q, err = %v", got, err)
	}
	// The store must live under the resolved data directory.
	if _, err := os.Stat(filepath.Join(dir, "secret.key")); err != nil {
		t.Fatalf("key file not created under CERAA_HOME: %v", err)
	}
}
