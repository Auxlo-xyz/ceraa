package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDataDirIsCeraa pins the single supported location.
func TestDataDirIsCeraa(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CERAA_HOME", "")
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
// candidate only. It is routinely set to unrelated directories, so honouring it
// for writes would silently relocate a live install's config, secrets and
// sessions.
func TestDataDirIgnoresXDG(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(t.TempDir(), "xdg")
	t.Setenv("HOME", home)
	t.Setenv("CERAA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "ceraa"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "ceraa")
	if got := DataDir(); got != want {
		t.Fatalf("DataDir = %q, want %q (XDG must not relocate writes)", got, want)
	}
	if !contains(ConfigCandidates(), filepath.Join(xdg, "ceraa", "config.json")) {
		t.Fatalf("XDG config candidate missing from %v", ConfigCandidates())
	}
}

// TestConfigCandidatesCoverTheDataDir pins that a config written by Save is
// always found by Load.
func TestConfigCandidatesCoverTheDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CERAA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	want := filepath.Join(DataDir(), "config.json")
	if !contains(ConfigCandidates(), want) {
		t.Fatalf("ConfigCandidates = %v, missing %q", ConfigCandidates(), want)
	}
}

// TestDefaultWorkspaceRoot pins the default location.
func TestDefaultWorkspaceRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := DefaultWorkspaceRoot(), filepath.Join(home, "ceraa", "workspace"); got != want {
		t.Fatalf("DefaultWorkspaceRoot = %q, want %q", got, want)
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
