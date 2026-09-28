package telegram

import (
	"os"
	"path/filepath"
	"testing"

	"ceraa/internal/config"
	"ceraa/internal/paths"
)

// TestMain points the whole package at a temporary data directory before any test
// runs.
//
// The provider selection is a file the menus write, and several tests write it.
// Without this, a test that does not stub the store reaches the real
// $CERAA_HOME: it happened, and it wrote a config.json naming provider "codex"
// and model "test/model" into the live install, which would then have pinned the
// running bot to a fixture. Individual tests stub what they touch, but a package
// default is the only thing that covers the ones that forget.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ceraa-telegram-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("CERAA_HOME", dir); err != nil {
		panic(err)
	}
	providers = newProviderState(config.NewStoreAt(filepath.Join(dir, "config.json")))
	os.Exit(m.Run())
}

// TestTheLiveInstallIsNotReachable guards the guard: if this fails, something in
// the package resolved a path outside the temporary directory and the tests are
// writing to the user's real state again.
func TestTheLiveInstallIsNotReachable(t *testing.T) {
	got := paths.DataDir()
	if got == os.Getenv("CERAA_HOME") {
		return
	}
	t.Fatalf("the package data dir %q is not the test directory %q; a test would write to the live install",
		got, os.Getenv("CERAA_HOME"))
}
