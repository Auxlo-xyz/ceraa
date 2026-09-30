package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ceraa/internal/provision"
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
// apart, and the failure is logged instead of being invisible. It says "your name
// is" rather than "you are" for the same reason the real prompt does: a model
// reading a one-line description of itself treats it as a role and answers
// identity questions from its model id instead.
const degraded = "Your name is Ceraa, a coding agent for software engineering."

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
			return resolveSearchTool(string(contents), candidate)
		}
	}
	fmt.Fprintf(os.Stderr,
		"ceraa: system prompt not found at %s (also tried the executable directory); using a minimal fallback. Run from the repository root or set CERAA_SYSTEM_PROMPT.\n",
		SystemPromptPath)
	return degraded
}

// searchLine is the literal-search instruction, and it is static.
//
// There is no environment branch and no placeholder. ripgrep is provisioned at
// startup, so by the time this reaches the model there is one correct line, and a
// prompt that never varies is a prompt that stays byte-identical in the cached
// prefix for every turn. The earlier version picked between rg and grep by
// probing PATH, which put a per-deployment value into the prefix to save a
// sentence.
const searchLine = `Use rg, which is provisioned for you: rg -n "pattern" PATH to find a literal with line ` +
	`numbers, rg --files PATH to list files, and -g to filter by type such as -g "*.ts". It respects ` +
	`.gitignore, so do not exclude directories by hand.`

// resolveSearchTool provisions ripgrep and substitutes the search instruction.
func resolveSearchTool(text, source string) string {
	// Provisioning runs here, before the prompt is handed to anything, and it is
	// not optional: the prompt names rg, so rg has to exist. A failure is
	// reported loudly rather than worked around, because a silent fallback to a
	// slower tool is how a deployment ends up quietly worse than it was configured
	// to be with nobody knowing why.
	if !provision.EnsureRipgrep() {
		fmt.Fprintf(os.Stderr,
			"ceraa: ripgrep could not be provisioned, and the prompt will name a tool that is not present: %s\n"+
				"        searches will fail until it is installed. On Debian or Ubuntu: apt-get install ripgrep\n",
			provision.ProvisionReason())
	}
	resolved := strings.ReplaceAll(text, "{{SEARCH}}", searchLine)
	// A placeholder left in place reaches the model as an instruction to search
	// for the literal string {{SEARCH}}, so it is reported rather than shipped.
	if strings.Contains(resolved, "{{") {
		fmt.Fprintf(os.Stderr,
			"ceraa: %s still contains an unresolved placeholder after substitution; check the spelling.\n", source)
	}
	return resolved
}

// Resolve returns the prompt as the model receives it.
//
// Exported so a test asserting on the shipped guidance reads what the model
// reads, rather than the file with a placeholder still in it.
func Resolve(text string) string { return resolveSearchTool(text, SystemPromptPath) }

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
