package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDataDirPrefersCeraa pins that a real Ceraa directory is authoritative.
func TestDataDirPrefersCeraa(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	// Both exist: Ceraa must win, or a rename would look like a data loss.
	if err := os.MkdirAll(filepath.Join(home, ".config", "ceraa"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "sooraa"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, want := DataDir(), filepath.Join(home, ".config", "ceraa"); got != want {
		t.Fatalf("DataDir = %q, want %q", got, want)
	}
	if UsingLegacyDataDir() {
		t.Fatal("must not report legacy when a Ceraa directory exists")
	}
}

// TestDataDirFallsBackToLegacy is the upgrade-safety property: a pre-rename
// install keeps reading its own config, secrets, sessions and log instead of
// silently starting empty.
func TestDataDirFallsBackToLegacy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if err := os.MkdirAll(filepath.Join(home, ".config", "sooraa"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, want := DataDir(), filepath.Join(home, ".config", "sooraa"); got != want {
		t.Fatalf("DataDir = %q, want the legacy %q", got, want)
	}
	if !UsingLegacyDataDir() {
		t.Fatal("must report the legacy directory so startup can say so")
	}
}

// TestDataDirDefaultsToCeraa covers a clean install.
func TestDataDirDefaultsToCeraa(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if got, want := DataDir(), filepath.Join(home, ".config", "ceraa"); got != want {
		t.Fatalf("DataDir = %q, want %q", got, want)
	}
}

// TestCeraaHomeWins pins the explicit override.
func TestCeraaHomeWins(t *testing.T) {
	home := t.TempDir()
	override := filepath.Join(t.TempDir(), "elsewhere")
	t.Setenv("HOME", home)
	t.Setenv("CERAA_HOME", override)
	if got := DataDir(); got != override {
		t.Fatalf("DataDir = %q, want %q", got, override)
	}
}

// TestDataDirIgnoresXDG pins a deliberate choice: XDG_CONFIG_HOME is a read
// candidate only. It is routinely set to unrelated directories (a tool's own
// config dir, for instance), so honouring it for writes would silently relocate
// a live install's config, secrets and sessions.
func TestDataDirIgnoresXDG(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(t.TempDir(), "xdg")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "ceraa"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "ceraa")
	if got := DataDir(); got != want {
		t.Fatalf("DataDir = %q, want %q (XDG must not relocate writes)", got, want)
	}
	// XDG is still a read candidate for config.json.
	if !contains(ConfigCandidates(), filepath.Join(xdg, "ceraa", "config.json")) {
		t.Fatalf("XDG config candidate missing from %v", ConfigCandidates())
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// TestDefaultWorkspaceRootFallsBackToLegacy keeps an existing workspace.
func TestDefaultWorkspaceRootFallsBackToLegacy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacy := filepath.Join(home, "sooraa", "workspace")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := DefaultWorkspaceRoot(); got != legacy {
		t.Fatalf("DefaultWorkspaceRoot = %q, want %q", got, legacy)
	}
	fresh := filepath.Join(home, "ceraa", "workspace")
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := DefaultWorkspaceRoot(); got != fresh {
		t.Fatalf("DefaultWorkspaceRoot = %q, want %q", got, fresh)
	}
}

// TestLegacyConfigCandidatesFindsUnmovedConfig pins that a config written
// pre-rename is still discoverable.
func TestLegacyConfigCandidatesFindsUnmovedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	want := filepath.Join(home, ".config", "sooraa", "config.json")
	for _, candidate := range LegacyConfigCandidates() {
		if candidate == want {
			return
		}
	}
	t.Fatalf("legacy candidates %v do not include %q", LegacyConfigCandidates(), want)
}
