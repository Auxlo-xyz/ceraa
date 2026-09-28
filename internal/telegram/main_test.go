package telegram

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/internal/config"
	"ceraa/internal/logger"
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
//
// CERAA_LOG_FILE is the same guard for the log. The logger resolves through
// $HOME, which this process shares with the running bot, so setting CERAA_HOME
// was not enough: the suite appended its own lines to the live bridge.log.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ceraa-telegram-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("CERAA_HOME", dir); err != nil {
		panic(err)
	}
	if err := os.Setenv("CERAA_LOG_FILE", filepath.Join(dir, "bridge.log")); err != nil {
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

// The log is the record of what a provider actually did, so the suite writing to
// it is the same defect as writing to the live config, and it is the one that
// quietly invalidates every other diagnosis drawn from the file.
func TestTheLiveLogIsNotReachable(t *testing.T) {
	want := os.Getenv("CERAA_LOG_FILE")
	if want == "" {
		t.Fatal("CERAA_LOG_FILE is unset, so the logger resolves through $HOME into the live log")
	}
	if !strings.HasPrefix(logger.Get().Path(), filepath.Dir(want)) {
		t.Errorf("the logger writes to %q, outside the test directory containing %q",
			logger.Get().Path(), want)
	}
}
