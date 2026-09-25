// Package paths resolves Ceraa's on-disk locations.
//
// The layout is fixed: $HOME/.config/ceraa for config, the encrypted credential
// store, sessions, memory, skills, and the log; $HOME/ceraa/workspace as the
// default workspace root. CERAA_HOME overrides the data directory.
//
// XDG_CONFIG_HOME is a config read candidate only, never a write target. It is
// routinely set to unrelated directories, so honouring it for writes would
// silently relocate a live install's data.
package paths

import (
	"os"
	"path/filepath"
)

// AppName is the product name, used in user-facing text.
const AppName = "Ceraa"

// DirName is the on-disk directory name. It is lowercase by convention, so it is
// kept separate from AppName: joining AppName would create ".config/Ceraa".
const DirName = "ceraa"

// DataDir returns the directory holding config, secrets, sessions, and logs.
func DataDir() string {
	if override := os.Getenv("CERAA_HOME"); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", DirName)
}

// DefaultWorkspaceRoot returns the fallback workspace root, derived from the
// installed binary's parent directory when it is unambiguous.
func DefaultWorkspaceRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "workspace"
	}
	return filepath.Join(home, DirName, "workspace")
}

// ConfigCandidates returns config.json locations in precedence order. XDG is
// included as a read candidate only; writes always go to DataDir.
func ConfigCandidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	candidates := []string{filepath.Join(DataDir(), "config.json")}
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		candidates = append(candidates, filepath.Join(base, DirName, "config.json"))
	}
	return append(candidates, filepath.Join(home, ".config", DirName, "config.json"), "./"+DirName+".json")
}
