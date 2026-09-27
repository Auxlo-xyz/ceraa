package menus

import (
	"os"
	"path/filepath"
	"testing"
)

// withActiveProvider points the runtime state at a temp data dir with one active
// provider, following the pattern the state tests already use.
func withActiveProvider(t *testing.T, providerID string) func() {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	previous := runtimeStatePath
	runtimeStatePath = filepath.Join(dir, "bridge-state.json")
	_ = os.WriteFile(runtimeStatePath, []byte(`{"provider":"`+providerID+`","model":"","provider_profiles":{}}`), 0o600)
	return func() { runtimeStatePath = previous }
}
