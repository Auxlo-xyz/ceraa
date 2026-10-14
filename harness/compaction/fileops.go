package compaction

import (
	"fmt"
	"sort"
	"strings"

	"ceraa/harness/sessionstore"
)

// Recorded file operations, appended to the Relevant Files section of a
// validated summary.
//
// The summariser writes that section from memory ("list files touched"), and
// memory is exactly what a compaction just discarded. The operation log is the
// durable answer to the same question, so the fold appends it as recorded
// facts beneath the model's recollection. The heading stays model-written and
// untouched: this appends inside the section, never renames it, and a summary
// without the heading passes through unchanged.
//
// Only file tools with explicit paths are counted. Shell is deliberately
// excluded: a bash call's files are unparseable in principle (heredocs,
// redirections, computed paths), and a list that guesses is worse than one
// that admits its boundary. Reads count as reads, writes and edits as writes,
// LSP queries as reads. Only completed operations count: a failed call touched
// nothing.
//
// The sets are cumulative over the session, not scoped to the folded span. A
// span-scoped set would need operation timestamps the store does not keep, and
// a second answer derived differently from the same log is how two lists drift
// apart quietly.
func RecordedFiles(ops []sessionstore.Operation) (reads, writes []string) {
	readSet := map[string]bool{}
	writeSet := map[string]bool{}
	for _, op := range ops {
		if op.Status != sessionstore.StatusCompleted {
			continue
		}
		path, _ := op.Args["path"].(string)
		if strings.TrimSpace(path) == "" {
			continue
		}
		switch op.Tool {
		case "read", "lsp_diagnostics", "lsp_definition", "lsp_references", "lsp_hover", "lsp_symbols":
			readSet[path] = true
		case "write", "edit_line", "batch_edit_file":
			writeSet[path] = true
		}
	}
	for path := range readSet {
		reads = append(reads, path)
	}
	for path := range writeSet {
		writes = append(writes, path)
	}
	sort.Strings(reads)
	sort.Strings(writes)
	return reads, writes
}

// MaxRecordedFiles caps the appended list. Twenty lines of paths is grounding;
// two hundred is a second transcript riding inside the summary.
const MaxRecordedFiles = 20

// AppendRecorded adds the recorded file sets to the Relevant Files section of
// a validated summary. It runs after validation and before persistence, so
// every copy of the summary (checkpoint, archive, Learn) carries the same
// facts. Empty sets leave the text byte-identical.
func AppendRecorded(summary string, reads, writes []string) string {
	entries := mergeRecorded(reads, writes)
	if len(entries) == 0 {
		return summary
	}
	lines := strings.Split(summary, "\n")
	heading := indexOfSummaryHeading(lines, "Relevant Files", 0)
	if heading < 0 {
		return summary
	}
	end := len(lines)
	for i := heading + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			end = i
			break
		}
	}
	recorded := make([]string, 0, len(entries)+1)
	recorded = append(recorded, "Recorded (tool log, not recollection):")
	shown := entries
	elided := 0
	if len(entries) > MaxRecordedFiles {
		shown = entries[:MaxRecordedFiles]
		elided = len(entries) - MaxRecordedFiles
	}
	recorded = append(recorded, shown...)
	if elided > 0 {
		recorded = append(recorded, fmt.Sprintf("…%d more recorded files elided", elided))
	}
	out := make([]string, 0, len(lines)+len(recorded))
	out = append(out, lines[:end]...)
	out = append(out, recorded...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n")
}

func mergeRecorded(reads, writes []string) []string {
	writeSet := map[string]bool{}
	for _, path := range writes {
		writeSet[path] = true
	}
	seen := map[string]bool{}
	var entries []string
	for _, path := range reads {
		marker := "read"
		if writeSet[path] {
			marker = "read, write"
		}
		seen[path] = true
		entries = append(entries, fmt.Sprintf("- %s (%s)", path, marker))
	}
	for _, path := range writes {
		if seen[path] {
			continue
		}
		entries = append(entries, fmt.Sprintf("- %s (write)", path))
	}
	sort.Strings(entries)
	return entries
}
