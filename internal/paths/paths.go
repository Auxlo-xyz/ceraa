// Package paths resolves Ceraa's on-disk locations.
//
// Ceraa was previously named Sooraa. The rename must not orphan an existing
// install: that directory holds the config, the encrypted credential store (and
// the AES key that decrypts it), sessions, memory, skills, and the log. So a
// legacy directory is still honoured when no Ceraa directory exists yet, and it
// is never deleted or moved automatically.
package paths

import (
	"os"
	"path/filepath"
)

// AppName is the product name.
const AppName = "Ceraa"

// legacyAppName is the pre-rename directory name, still read for compatibility.
const legacyAppName = "sooraa"

// legacyWorkspaceDir is the pre-rename default workspace root.
const legacyWorkspaceDir = "sooraa"

// DataDir returns the directory holding config, secrets, sessions, and logs.
//
// The location is $HOME/.config/ceraa, matching what the project has always
// used. XDG_CONFIG_HOME is deliberately NOT consulted here: it is only a config
// *read* candidate, and honouring it for writes would silently relocate a live
// install's data whenever the variable happens to point somewhere unexpected.
//
// CERAA_HOME wins when set. Otherwise the Ceraa directory is used, EXCEPT when
// it does not exist and a legacy Sooraa directory does: in that case the legacy
// directory is returned so an upgraded install keeps reading its own data. Once
// a Ceraa directory exists it is authoritative and the legacy one is ignored.
func DataDir() string {
	if override := os.Getenv("CERAA_HOME"); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	candidate := filepath.Join(home, ".config", "ceraa")
	if dirExists(candidate) {
		return candidate
	}
	if legacy := filepath.Join(home, ".config", legacyAppName); dirExists(legacy) {
		return legacy
	}
	return candidate
}

// UsingLegacyDataDir reports whether DataDir resolved to the pre-rename
// directory, so startup can tell the operator their install was carried over.
func UsingLegacyDataDir() bool {
	dir := DataDir()
	return filepath.Base(dir) == legacyAppName
}

// DefaultWorkspaceRoot returns the fallback workspace root, honouring a legacy
// directory that already exists for the same reason as DataDir.
func DefaultWorkspaceRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "workspace"
	}
	candidate := filepath.Join(home, "ceraa", "workspace")
	if dirExists(candidate) {
		return candidate
	}
	if legacy := filepath.Join(home, legacyWorkspaceDir, "workspace"); dirExists(legacy) {
		return legacy
	}
	return candidate
}

// ConfigCandidates returns config.json locations in precedence order. XDG is
// included as a read candidate only; writes always go to DataDir. The legacy
// locations are appended so an install that has not been moved still finds its
// config, and a Ceraa config always takes precedence over a pre-rename one.
func ConfigCandidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	candidates := []string{filepath.Join(DataDir(), "config.json")}
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		candidates = append(candidates, filepath.Join(base, "ceraa", "config.json"))
	}
	candidates = append(candidates, filepath.Join(home, ".config", "ceraa", "config.json"), "./ceraa.json")
	return append(candidates, LegacyConfigCandidates()...)
}

// LegacyConfigCandidates returns pre-rename config.json locations, so a renamed
// install still finds a config that was never moved.
func LegacyConfigCandidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	var candidates []string
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		candidates = append(candidates, filepath.Join(base, legacyAppName, "config.json"))
	}
	return append(candidates, filepath.Join(home, ".config", legacyAppName, "config.json"))
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
