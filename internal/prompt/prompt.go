package prompt

import (
	"fmt"
	"os"
	"os/exec"
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

// searchPlaceholder carries the literal-search instruction.
//
// It is substituted once, at startup, because the search tool differs by
// deployment and this bot runs for several users whose images are not the same
// image. Naming ripgrep to a deployment that does not have it teaches the model
// to reach for a binary that will fail, which is the same mistake as naming a
// tool that has been removed.
//
// Resolving at startup rather than per request is what keeps the cached prefix
// stable: one process serves one prompt, so the line is byte-identical for every
// turn of every session that process handles, and each deployment gets the line
// that is true for it.
const searchPlaceholder = "{{SEARCH}}"

const (
	// searchLineRipgrep names the flags that matter rather than the binary alone,
	// because the model otherwise invents its own invocation and loses the
	// gitignore behaviour that makes it worth having.
	searchLineRipgrep = `Use rg, which is installed here: rg -n "pattern" PATH to find a literal with line numbers, ` +
		`rg --files PATH to list files, and rg -n --glob "!node_modules" to skip build output. ` +
		`It already skips anything in .gitignore, so do not exclude directories by hand.`

	// searchLineGrep is the fallback for a deployment without ripgrep, and it
	// carries the exclusions explicitly because grep has no such default.
	searchLineGrep = `find . -name "*.go" -not -path "*/node_modules/*" to list, and ` +
		`grep -rn --exclude-dir=node_modules --exclude-dir=.git "pattern" PATH to find a literal.`
)

// resolveSearchTool substitutes the search instruction for whichever tool this
// deployment actually has.
func resolveSearchTool(text, source string) string {
	if !strings.Contains(text, searchPlaceholder) {
		return text
	}
	line := searchLineGrep
	if _, err := exec.LookPath("rg"); err == nil {
		line = searchLineRipgrep
	}
	resolved := strings.ReplaceAll(text, searchPlaceholder, line)
	// A placeholder left in place would reach the model as literal text, which
	// reads as an instruction to search for the string "{{SEARCH}}". Failing here
	// is better than that, and better still than shipping a typo unnoticed.
	if strings.Contains(resolved, "{{") {
		fmt.Fprintf(os.Stderr,
			"ceraa: %s still contains an unresolved placeholder after substitution; check the spelling.\n", source)
	}
	return resolved
}

// Resolve substitutes the search instruction the way startup does.
//
// Exported so a test that asserts on the shipped guidance reads the prompt the
// model actually receives. Two tests failed on the raw file once the search line
// moved behind a placeholder, and re-implementing the substitution in each test
// is how they would drift apart.
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
