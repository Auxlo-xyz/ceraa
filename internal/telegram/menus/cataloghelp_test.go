package menus

import (
	"os"
	"path/filepath"
	"testing"
)

// withActiveProvider points the config at a temp data dir with one active provider.
//
// The selection is written to config.json, which is where it now lives. The
// previous version seeded bridge-state.json with `{"provider":...}`, a key this
// build no longer reads, so the helper quietly stopped selecting anything and
// every assertion after it was testing the wrong thing.
func withActiveProvider(t *testing.T, providerID string) func() {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)

	previousStatePath := runtimeStatePath
	runtimeStatePath = filepath.Join(dir, "bridge-state.json")
	if err := os.WriteFile(runtimeStatePath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	body := `{"active_provider":"` + providerID + `"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return func() { runtimeStatePath = previousStatePath }
}
