package coordinator

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ceraa/harness/tool"
)

// maxCommandTargets bounds the work and the noise.
//
// A shell command is a bag of tokens and this reads it as one. The bound is what
// keeps that honest: a build that names three hundred files contributes three, and
// a memory about a dependency directory does not become a wall of annotations.
const maxCommandTargets = 8

// segmentSeparators split one command from the next. Splitting on these first is
// what makes "the first token of a segment is the program" available, which is the
// single most effective filter here.
//
// Redirection is deliberately NOT here. `echo hi > out/log.txt` has to yield
// out/log.txt, and if `>` split the segments then that word would arrive first in
// its segment and be discarded as a program name - the opposite of the truth.
const segmentSeparators = ";|&\n"

// tokenSeparators split a segment into its words.
const tokenSeparators = " \t,<>(){}"

// pathsFromCommand extracts the file paths a shell command is about.
//
// Three filters, in order, and each exists because containment alone was not
// enough. Resolving a token against the workspace root makes the command's own
// name look like a path inside it: `rm main.go` yields both /srv/app/rm and
// /srv/app/main.go, and `go build ./...` yields /srv/app/go. Every token in a
// command is "inside the workspace" once it is joined onto the root, so
// containment filters nothing at all on its own.
//
//  1. The first token of each segment is the program being run, never a target.
//  2. A glob is a pattern, not a file, and a memory scoped to one is meaningless.
//  3. What is left has to be a file that exists, or a filename. This is the filter
//     that does the work, and it is a fact about the filesystem rather than a
//     judgement about a string.
//
// The failure mode being bought down is a missed annotation, not a wrong one: a
// token this skips is simply delivered on a later operation, and the memory is
// still in the store either way.
func pathsFromCommand(command, workDir string) []string {
	command = strings.TrimSpace(command)
	if command == "" || workDir == "" {
		return nil
	}
	root := filepath.Clean(workDir)
	seen := map[string]bool{}
	var out []string

	add := func(candidate string) bool {
		candidate = filepath.Clean(candidate)
		// The containment check is still required, and still does real work: it
		// drops absolute paths elsewhere and drops ../.. escapes, without either
		// being special cased.
		if candidate != root && !strings.HasPrefix(candidate, root+string(filepath.Separator)) {
			return false
		}
		if seen[candidate] {
			return false
		}
		seen[candidate] = true
		out = append(out, candidate)
		return len(out) >= maxCommandTargets
	}

	for _, segment := range strings.FieldsFunc(command, func(r rune) bool {
		return strings.ContainsRune(segmentSeparators, r)
	}) {
		words := strings.FieldsFunc(segment, func(r rune) bool {
			return strings.ContainsRune(tokenSeparators, r)
		})
		for i, word := range words {
			// The program. `rm`, `sed`, `python3`, `git` - never the file.
			if i == 0 {
				continue
			}
			word = strings.Trim(word, `"'`)
			if word == "" || strings.HasPrefix(word, "-") {
				continue
			}
			if strings.Contains(word, "://") {
				continue
			}
			// A glob names a pattern. A memory scoped to /srv/*.log would match
			// nothing here and read as though it had.
			if strings.ContainsAny(word, "*?[]") {
				continue
			}
			// `go build ./...` yields a token whose base is three dots, and
			// filepath.Ext reads the trailing ".." as an extension, so it would
			// otherwise pass as a filename.
			if !hasAlphanumeric(filepath.Base(word)) {
				continue
			}
			candidate := word
			if !filepath.IsAbs(candidate) {
				candidate = filepath.Join(root, candidate)
			}
			candidate = filepath.Clean(candidate)
			// The filter that does the work: a file that is there, or a name that
			// looks like a file. Existence covers extensionless files like a
			// Makefile; the extension covers the file a command is about to create.
			if !looksLikeTarget(candidate) {
				continue
			}
			if add(candidate) {
				return sortedTargets(out)
			}
		}
	}
	return sortedTargets(out)
}

// hasAlphanumeric rejects a base name made only of punctuation.
func hasAlphanumeric(name string) bool {
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return true
		}
	}
	return false
}

// looksLikeTarget is "this is a file, or a filename", decided by the filesystem
// rather than by parsing.
func looksLikeTarget(path string) bool {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return true
	}
	// A directory is not itself the thing a command edits, and treating it as one
	// would put a memory about src/ on every read of anything under it.
	return filepath.Ext(path) != ""
}

func sortedTargets(out []string) []string {
	// Sorted so the same command always produces the same list. A command whose
	// targets reorder between two identical operations would deliver a different
	// annotation each time, which makes the delivery impossible to compare.
	sort.Strings(out)
	return out
}

// targetsForSpec is the set of paths an operation is about, whatever tool it is.
//
// Eight tools carry an explicit path. A shell command does not, and it is the
// majority of real operations, so the command form is the normal case and the
// explicit path is the convenient one rather than the other way round.
//
// The workDir is a parameter rather than s.workDir because the instruction
// observer calls this from outside any sidekick state. The sidekick method below
// is the wrapper, so there is still exactly one implementation.
//
// specType is UNUSED, and that is deliberate rather than an oversight to be
// tidied up. It branches on the shape of args alone: args["path"] if present,
// otherwise args["command"]. Every caller so far has an operation whose args
// already say which of the two forms it is in, so a spec-type table here would
// be a second place that has to be right about how each tool spells its
// arguments, and it would be a second place to be wrong. If a tool ever arrives
// with a `path` it does not act on - a destination, a search root - then the
// dispatch belongs here, and the sidekick's existing tests are what should move
// when it does.
func targetsForSpec(specType string, workDir string, args map[string]any) []string {
	if path, _ := args["path"].(string); strings.TrimSpace(path) != "" {
		resolved, err := tool.ResolvePathFrom(workDir, path)
		if err != nil {
			return nil
		}
		return []string{filepath.Clean(resolved)}
	}
	command, _ := args["command"].(string)
	return pathsFromCommand(command, workDir)
}
