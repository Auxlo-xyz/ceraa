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

// FilesDir is where Telegram uploads land: photos, documents, voice notes.
//
// It used to be /tmp/ceraa-uploads, and /tmp is wiped on reboot, so saved
// documents silently vanished along with every extraction done from them. It
// then moved to <repo>/files, which put user uploads inside the source tree
// the bot is forbidden to write to. Production pins CERAA_FILES_DIR to
// /workspace/ceraa/files, which is data, not source, and the override is what
// tests use to stay hermetic. The exe-anchored fallback below is for shell
// and dev use only, and must never become the live bot's location again.
func FilesDir() string {
	if override := os.Getenv("CERAA_FILES_DIR"); override != "" {
		return override
	}
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); filepath.Base(dir) == "bin" {
			return filepath.Join(filepath.Dir(dir), "files")
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	return filepath.Join(cwd, "files")
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
// They are consulted **only when CERAA_HOME is unset**. Once it is set, the data
// directory is the whole truth: that is what the variable means, and honouring
// the fallbacks anyway made CERAA_HOME a partial override rather than a switch.
// The concrete failure: with CERAA_HOME pointing at a scratch directory,
// $HOME/.config/ceraa/config.json still resolved and won, so a process meant to be
// isolated read the live install's providers anyway. A test hit it, and so would
// anyone running a second install beside their first.
//
// Kept for an install that has not been moved. XDG_CONFIG_HOME is here for the
// same reason: it is routinely set to unrelated directories, so honouring it for
// writes would silently relocate a live install's data.
func LegacyConfigCandidates() []string {
	if os.Getenv("CERAA_HOME") != "" {
		return nil
	}
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
