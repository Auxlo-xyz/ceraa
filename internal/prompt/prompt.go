package prompt

import (
	"fmt"
	"os"
	"strings"

	sourceprompt "ceraa/prompt"
)

// System is the system prompt handed to the model.
//
// The release contains this prompt in the binary. It has no runtime override.
var System string

// degraded is used only as a defensive guard if the embedded asset is empty.
const degraded = "Your name is Ceraa, a coding agent for software engineering."

// SystemPromptPath is the source path used by the repository and override docs.
const SystemPromptPath = "prompt/system.txt"

func init() {
	System = load()
}

// load always uses the prompt embedded in the binary. Prompt text is a
// performance-critical part of Ceraa's behavior, so it has one authoritative
// released form and cannot be replaced through an environment variable.
func load() string {
	if len(strings.TrimSpace(string(sourceprompt.System))) > 0 {
		return resolveSearchTool(string(sourceprompt.System), "embedded prompt")
	}
	fmt.Fprintln(os.Stderr, "ceraa: embedded system prompt is empty; using a minimal fallback.")
	return degraded
}

// searchLine is the literal-search instruction, and it is static.
const searchLine = `Use rg, which is provisioned for you: rg -n "pattern" PATH to find a literal with line ` +
	`numbers, rg --files PATH to list files, and -g to filter by type such as -g "*.ts". It respects ` +
	`.gitignore, so do not exclude directories by hand.`

func resolveSearchTool(text, source string) string {
	resolved := strings.ReplaceAll(text, "{{SEARCH}}", searchLine)
	if strings.Contains(resolved, "{{") {
		fmt.Fprintf(os.Stderr,
			"ceraa: %s still contains an unresolved placeholder after substitution; check the spelling.\n", source)
	}
	return resolved
}

func Resolve(text string) string { return resolveSearchTool(text, SystemPromptPath) }
