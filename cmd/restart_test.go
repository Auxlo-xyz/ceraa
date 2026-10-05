package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The CLI restart waits on the pid file, not the process: the old image
// removes it in its shutdown defer and the new image rewrites it on claim,
// so a newer file is the restart completing. A file that never gets newer is
// a restart that never came back.
func TestWaitForPidRewriteSeesTheNewClaim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ceraa.pid")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	since := before.ModTime()
	go func() {
		time.Sleep(100 * time.Millisecond)
		os.Remove(path)
		time.Sleep(100 * time.Millisecond)
		os.WriteFile(path, []byte("new"), 0o644)
	}()
	if err := waitForPidRewrite(path, since, 5*time.Second); err != nil {
		t.Errorf("wait missed the rewrite: %s", err)
	}
}

func TestWaitForPidRewriteTimesOut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ceraa.pid")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitForPidRewrite(path, before.ModTime(), 300*time.Millisecond); err == nil {
		t.Error("wait reported a restart nobody performed")
	}
}

// Restart must resolve as a subcommand, or `ceraa restart` falls through to
// the model as a prompt and spends an inference call on a one-word slip.
func TestRestartSubcommandRegistered(t *testing.T) {
	registerSubcommands()
	for _, sub := range root.Commands() {
		if sub.Name() == "restart" {
			return
		}
	}
	t.Error("restart not registered as a subcommand")
}
