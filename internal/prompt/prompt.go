package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// System is the system prompt handed to the model.
//
// There is exactly one copy, at prompt/system.txt, and it is read from disk
// rather than embedded. An earlier version embedded internal/prompt/system.txt
// and then silently overrode it with the file on disk, which left two copies
// that had to be kept in sync and made an edit to the embedded one look like it
// had been lost. The canonical location is the file named below.
//
// Reading at runtime means editing the prompt needs a restart but not a rebuild.
var System string

// degraded is used only when prompt/system.txt cannot be read. It is a minimal
// identity rather than a second copy of the prompt, so the two cannot drift
// apart, and the failure is logged instead of being invisible.
const degraded = "You are Ceraa, an interactive coding agent for software engineering tasks."

// SystemPromptPath is the canonical location, relative to the working directory.
const SystemPromptPath = "prompt/system.txt"

func init() {
	System = load()
}

// load reads the prompt, falling back to a minimal identity when it is missing
// or unreadable.
//
// The path is resolved against the working directory first and then against the
// directory holding the executable, so the prompt is still found when the bot is
// launched from somewhere other than the repository root. A relative read alone
// silently produced the embedded copy for exactly that reason.
func load() string {
	for _, candidate := range candidates() {
		contents, err := os.ReadFile(candidate)
		if err == nil && len(strings.TrimSpace(string(contents))) > 0 {
			return string(contents)
		}
	}
	fmt.Fprintf(os.Stderr,
		"ceraa: system prompt not found at %s (also tried the executable directory); using a minimal fallback. Run from the repository root or set CERAA_SYSTEM_PROMPT.\n",
		SystemPromptPath)
	return degraded
}

func candidates() []string {
	paths := []string{SystemPromptPath}
	if override := os.Getenv("CERAA_SYSTEM_PROMPT"); override != "" {
		// An override takes precedence, so it is tried first.
		paths = append([]string{override}, paths...)
	}
	if executable, err := os.Executable(); err == nil {
		directory := filepath.Dir(executable)
		paths = append(paths,
			filepath.Join(directory, SystemPromptPath),
			filepath.Join(directory, "..", SystemPromptPath),
		)
	}
	return paths
}
