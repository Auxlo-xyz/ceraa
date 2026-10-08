package memory

import (
	"html"
	"os"
	"strings"
)

// Bond is the user-level counterpart to project memory. Project files
// describe the work; the bond describes the person, and it is the same
// person in every project, so it lives once beside the config rather than
// once per checkout. Maintained by the operator (Sidekick), never by the
// agent: the law below is rendered into the prompt with the file, so the
// rule travels with the content instead of living in a prompt somewhere
// the file outlives.
const bondLaw = "BOND.md is Sidekick-maintained: read it, never write it. Propose changes in chat; only the operator commits them."

// LiveBond returns a renderer that re-reads the bond file on every call,
// like Live does for project memory. A missing file renders nothing and no
// error: a first run has no bond yet, and that is not a failure. Trimming
// and escaping mirror RenderFile; only the section tag differs, because a
// bond labelled as a memory file would read as project context.
func LiveBond(filePath string, maxBytes int) func() (string, error) {
	return func() (string, error) {
		if filePath == "" {
			return "", nil
		}
		raw, err := os.ReadFile(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				return "", nil
			}
			return "", err
		}
		body := strings.TrimSpace(string(raw))
		if body == "" {
			return "", nil
		}
		const open = "<bond>\n"
		const tail = "\n" + bondLaw + "\n</bond>\n"
		// The law is load-bearing, so truncation takes from the body and
		// never from it: a bond over budget degrades to stale-but-present
		// rules, not to rules without the read-only law.
		entry := open + html.EscapeString(body) + tail
		if maxBytes > 0 && len(entry) > maxBytes {
			entry = open + truncateUTF8(html.EscapeString(body), maxBytes-len(open)-len(tail)) + tail
		}
		return entry, nil
	}
}

// Combine chains renderers in order: bond first, so identity and standing
// rules precede project memory in the prompt. A failing source contributes
// nothing but does not blank the others; the first error is reported.
func Combine(renderers ...func() (string, error)) func() (string, error) {
	return func() (string, error) {
		var parts []string
		var firstErr error
		for _, render := range renderers {
			if render == nil {
				continue
			}
			rendered, err := render()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			if rendered != "" {
				parts = append(parts, rendered)
			}
		}
		if len(parts) == 0 {
			return "", firstErr
		}
		return strings.Join(parts, "\n") + "\n", firstErr
	}
}
