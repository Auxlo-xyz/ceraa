package operation

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Locating old_text, and turning a failed match into something the caller can act
// on without reading the file.
//
// The tools replace by exact text because a shell edit succeeds whether or not the
// file said what the model assumed, so a stale read becomes a silent corruption.
// That safety is the reason the interface is what it is, and it is kept here: the
// text must still be present, and exactly one occurrence is replaced.
//
// What was missing was a way back from a failed match. "found 3 occurrences" is
// not actionable, because it says how many and not where, so the only route to a
// fix was a read round trip or a guess at more context. Everything here exists to
// close that: the line of every occurrence, a way to name which one, and a hint
// when the text is present but differs only in whitespace.

// editMatch is where old_text was found in a file.
type editMatch struct {
	// count is how many times old_text occurs.
	count int
	// offsets are the byte offsets of those occurrences, in order.
	offsets []int
}

// locate finds every occurrence of oldText in content.
func locate(content, oldText string) editMatch {
	match := editMatch{}
	if oldText == "" {
		return match
	}
	for from := 0; ; {
		at := strings.Index(content[from:], oldText)
		if at < 0 {
			break
		}
		match.offsets = append(match.offsets, from+at)
		from += at + len(oldText)
	}
	match.count = len(match.offsets)
	return match
}

// lineAt is the 1-based line number of a byte offset.
//
// Counted rather than indexed, because there are at most a handful of offsets and
// a line-start table would be more code than the answer is worth.
func lineAt(content string, offset int) int {
	if offset > len(content) {
		offset = len(content)
	}
	return 1 + strings.Count(content[:offset], "\n")
}

// lineList renders every occurrence's line, so a caller picking one does not have
// to read the file to find out which is which.
func (m editMatch) lineList(content string) string {
	lines := make([]string, 0, len(m.offsets))
	for _, offset := range m.offsets {
		lines = append(lines, fmt.Sprintf("%d", lineAt(content, offset)))
	}
	return strings.Join(lines, ", ")
}

// occurrenceArg is the 1-based occurrence the caller asked for. Zero means none
// was asked for, which is the original contract: exactly one match, no choice.
func occurrenceArg(args map[string]any) int {
	switch typed := args["occurrence"].(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	default:
		return 0
	}
}

// resolve picks the byte offset to replace, or explains why it cannot.
//
// Two rules, and the second is the one that keeps the safety property intact:
// without an explicit occurrence the text must be unique, exactly as before, and
// with one it must still be present and the requested one must exist. Nothing here
// can report success for a replacement that did not happen.
func (m editMatch) resolve(content, file, oldText string, occurrence int) (int, string) {
	switch {
	case m.count == 0:
		hint := whitespaceHint(content, oldText)
		if hint > 0 {
			return 0, fmt.Sprintf("old_text not found in %s; it differs only in whitespace from line %d, so check the indentation or the trailing space", file, hint)
		}
		return 0, fmt.Sprintf("old_text not found in %s", file)
	case occurrence == 0:
		if m.count == 1 {
			return m.offsets[0], ""
		}
		return 0, fmt.Sprintf(
			"old_text occurs %d times in %s, at lines %s; add more surrounding context to make it unique, or pass occurrence to choose one of them",
			m.count, file, m.lineList(content))
	case occurrence < 1:
		return 0, fmt.Sprintf("occurrence is %d; it is 1-based, so it must be at least 1", occurrence)
	case occurrence > m.count:
		return 0, fmt.Sprintf("occurrence %d was asked for but old_text occurs %d times in %s, at lines %s",
			occurrence, m.count, file, m.lineList(content))
	default:
		return m.offsets[occurrence-1], ""
	}
}

// whitespaceHint reports the 1-based line where oldText appears once whitespace
// runs are collapsed and ends are trimmed, or 0 when it does not.
//
// A fact about the file rather than a guess about intent. Trailing whitespace, a
// tab written for spaces, or a missing final newline produce a count of zero and
// an error that tells the model nothing, which is the most common way this fails
// and the easiest to fix once named.
func whitespaceHint(content, oldText string) int {
	want := normaliseWhitespace(oldText)
	if want == "" {
		return 0
	}
	lines := strings.Split(content, "\n")
	wantLines := strings.Split(want, "\n")
	if len(wantLines) > len(lines) {
		return 0
	}
	for i := 0; i+len(wantLines) <= len(lines); i++ {
		if strings.Join(normaliseLines(lines[i:i+len(wantLines)]), "\n") == want {
			return i + 1
		}
	}
	return 0
}

func normaliseWhitespace(value string) string {
	return strings.Join(normaliseLines(strings.Split(value, "\n")), "\n")
}

func normaliseLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, strings.Join(strings.Fields(line), " "))
	}
	return out
}

// editFailure is one batch edit that could not be applied, described well enough
// that fixing it needs no read.
type editFailure struct {
	index  int
	reason string
}

// applyEdits applies a validated edit set to content, all or nothing.
//
// Every edit is resolved against the untouched content first, and every failure
// is reported at once. The previous version stopped at the first, so a batch of
// five with two problems cost two round trips to discover, which is exactly what a
// batch exists to avoid.
func applyEdits(content, file string, edits []map[string]any) (string, []editFailure, bool) {
	var failures []editFailure
	// Resolved against the original content, and the original content is what each
	// replacement is applied to, so an edit never has to match a sibling's output.
	// That is the property that makes a set with overlapping regions safe.
	offsets := make([]int, len(edits))
	lengths := make([]int, len(edits))
	for i, pair := range edits {
		oldText, _ := pair["old_text"].(string)
		match := locate(content, oldText)
		offset, reason := match.resolve(content, file, oldText, occurrenceArg(pair))
		if reason != "" {
			failures = append(failures, editFailure{index: i, reason: reason})
			continue
		}
		offsets[i] = offset
		lengths[i] = len(oldText)
	}
	if len(failures) > 0 {
		return "", failures, false
	}

	// Applied back to front so earlier offsets stay valid as later ones change
	// the content length.
	order := make([]int, len(edits))
	for i := range order {
		order[i] = i
	}
	for i := 1; i < len(order); i++ {
		for j := i; j > 0 && offsets[order[j]] > offsets[order[j-1]]; j-- {
			order[j], order[j-1] = order[j-1], order[j]
		}
	}
	updated := content
	for _, i := range order {
		newText, _ := edits[i]["new_text"].(string)
		updated = updated[:offsets[i]] + newText + updated[offsets[i]+lengths[i]:]
	}
	return updated, nil, true
}

// batchFailureText renders every failure in one message, so one reply is enough
// to fix all of them.
//
// total is the size of the batch, not the size of the failure list, so the model
// can tell "one of these five is wrong" from "this one is wrong" and keep going
// on the four that were fine.
func batchFailureText(file string, total int, failures []editFailure) string {
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		parts = append(parts, fmt.Sprintf("edit %d: %s", failure.index, failure.reason))
	}
	// The noun agrees with the failure count, and the batch size is always the
	// plural one. "1 of 2 edit did not match" is what a single shared noun
	// produces, and it reads as a bug in the tool.
	if len(failures) == 1 && total == 1 {
		return fmt.Sprintf("the edit in %s did not match; nothing was written:\n  %s",
			file, strings.Join(parts, "\n  "))
	}
	return fmt.Sprintf("%d of the %d edits in %s did not match; nothing was written:\n  %s",
		len(failures), total, file, strings.Join(parts, "\n  "))
}

// editFileLabel is the name used in a message. The basename, because a full path
// in an error is noise the model has to read past to find the useful part.
func editFileLabel(path string) string {
	if base := filepath.Base(path); base != "" {
		return base
	}
	return path
}
