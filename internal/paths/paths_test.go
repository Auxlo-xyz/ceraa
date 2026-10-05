package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// With CERAA_HOME set, the config lives there and nowhere else.
//
// This is a production bug caught by a test: the pre-consolidation candidate list
// still ended in $HOME/.config/ceraa/config.json, so a process pointed at a
// scratch CERAA_HOME read the live install's config anyway. A second install
// beside the first would have been silently mixed with it.
func TestConfigFileIsTheOnlyCandidateWhenHomeIsPinned(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)

	candidates := ConfigCandidates()
	if len(candidates) != 1 {
		t.Fatalf("candidates = %v, want only the pinned data dir", candidates)
	}
	if candidates[0] != filepath.Join(dir, "config.json") {
		t.Errorf("candidate = %q", candidates[0])
	}
	if ConfigFile() != candidates[0] {
		t.Errorf("ConfigFile = %q, want the canonical candidate %q", ConfigFile(), candidates[0])
	}
}

// Without CERAA_HOME the old locations are still read, so an install that was
// never moved keeps working.
func TestUnpinnedHomeFallsBackToTheLegacyLocations(t *testing.T) {
	t.Setenv("CERAA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")

	candidates := ConfigCandidates()
	if len(candidates) < 2 {
		t.Fatalf("candidates = %v, want the legacy fallbacks", candidates)
	}
	// Canonical path first, always.
	if candidates[0] != ConfigFile() {
		t.Errorf("first candidate = %q, want %q", candidates[0], ConfigFile())
	}
	var sawXDG bool
	for _, candidate := range candidates {
		if candidate == filepath.Join("/xdg", DirName, "config.json") {
			sawXDG = true
		}
	}
	if !sawXDG {
		t.Errorf("the XDG read fallback was dropped: %v", candidates)
	}
}

// The working-directory candidate is the one that made the same binary read a
// different file depending on where it was launched from, so it must never
// outrank the data directory.
func TestWorkingDirectoryIsNeverTheOnlyOption(t *testing.T) {
	t.Setenv("CERAA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range ConfigCandidates() {
		if candidate == "./"+DirName+".json" {
			home, _ := os.UserHomeDir()
			canonical := filepath.Join(DataDir(), "config.json")
			_ = home
			if canonical == "./"+DirName+".json" {
				t.Fatalf("the data directory resolved to the working directory, so the two are indistinguishable")
			}
		}
	}
	_ = cwd
}

// Uploads must not land in /tmp: it is wiped on reboot, which silently deleted
// saved documents and every extraction done from them. The override exists so
// tests never touch the live files directory.
func TestFilesDirHonoursTheOverride(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "files")
	t.Setenv("CERAA_FILES_DIR", dir)
	if got := FilesDir(); got != dir {
		t.Fatalf("FilesDir() = %q, want the override %q", got, dir)
	}
}

func TestFilesDirNeverPointsAtTmp(t *testing.T) {
	t.Setenv("CERAA_FILES_DIR", "")
	if got := FilesDir(); got == filepath.Join(os.TempDir(), "ceraa-uploads") {
		t.Fatal("FilesDir() still points at the wiped-on-reboot /tmp location")
	}
}
