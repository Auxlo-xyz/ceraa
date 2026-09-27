package telegram

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain points the package at a throwaway data directory before any test runs.
//
// The telegram package holds its bridge state in a package-level var built from
// paths.DataDir(), so without this every test read the live install. That made a
// test asserting "no provider is configured" pass or fail depending on what the
// developer's install happened to have active, and it is how a test managed to
// activate a provider on the running bot. Reassigning defaultState here is
// deliberate: setting the environment variable alone is too late, because the
// package var is initialised before TestMain runs.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ceraa-telegram-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("CERAA_HOME", dir)
	defaultState = loadBridgeStateFromFile(filepath.Join(dir, "bridge-state.json"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
