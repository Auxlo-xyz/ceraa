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

// ConfigFile is the one place config.json is read from and written to.
//
// There is exactly one, and it is anchored to the data directory rather than to
// the environment or the working directory. The previous candidate list ended in
// "./ceraa.json", so the same binary read a different file depending on where it
// was launched from: the bot runs with its working directory pinned to the repo
// and a shell run read whatever sat next to it. Nothing warned, because reading a
// file that does not exist is not an error.
func ConfigFile() string { return filepath.Join(DataDir(), "config.json") }

// LegacyConfigCandidates are the pre-consolidation read locations, in the order
// they used to win.
//
// Kept as read-only fallbacks so an existing install is not orphaned by the path
// change, and never written to. XDG_CONFIG_HOME is here for the same reason: it
// is routinely set to unrelated directories, so honouring it for writes would
// silently relocate a live install's data.
func LegacyConfigCandidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	candidates := make([]string, 0, 3)
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		candidates = append(candidates, filepath.Join(base, DirName, "config.json"))
	}
	return append(candidates, filepath.Join(home, ".config", DirName, "config.json"), "./"+DirName+".json")
}

// ConfigCandidates returns every location config.json may be read from, canonical
// path first.
//
// Prefer ConfigFile for anything that writes or reports a location. This exists
// so loadFile can migrate a file written before the path was fixed.
func ConfigCandidates() []string {
	return append([]string{ConfigFile()}, LegacyConfigCandidates()...)
}
