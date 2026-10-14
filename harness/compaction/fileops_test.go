package compaction

import (
	"strings"
	"testing"

	"ceraa/harness/sessionstore"
)

func fileOp(tool, path, status string) sessionstore.Operation {
	return sessionstore.Operation{
		ID:     tool + ":" + path,
		Tool:   tool,
		Args:   map[string]any{"path": path},
		Status: sessionstore.OperationStatus(status),
	}
}

func TestRecordedFilesCountsCompletedFileToolsOnly(t *testing.T) {
	ops := []sessionstore.Operation{
		fileOp("read", "a.go", "completed"),
		fileOp("read", "a.go", "completed"),
		fileOp("edit_line", "a.go", "completed"),
		fileOp("write", "b.go", "completed"),
		fileOp("read", "d.go", "failed"),
		fileOp("bash", "e.go", "completed"),
		fileOp("question", "", "completed"),
	}
	reads, writes := RecordedFiles(ops)
	if len(reads) != 1 || reads[0] != "a.go" {
		t.Errorf("reads = %v, want [a.go] deduped and sorted", reads)
	}
	if len(writes) != 2 || writes[0] != "a.go" || writes[1] != "b.go" {
		t.Errorf("writes = %v, want [a.go b.go]", writes)
	}
}

// Shell calls name no path the log can trust, and a failed call touched
// nothing. Both stay out, or the recorded list starts asserting work that
// never happened.
func TestRecordedFilesExcludesShellAndFailures(t *testing.T) {
	ops := []sessionstore.Operation{
		{ID: "1", Tool: "bash", Args: map[string]any{"command": "rg foo"}, Status: sessionstore.StatusCompleted},
		fileOp("write", "b.go", "failed"),
	}
	reads, writes := RecordedFiles(ops)
	if len(reads) != 0 || len(writes) != 0 {
		t.Errorf("reads = %v writes = %v, want both empty", reads, writes)
	}
}

func TestAppendRecordedStaysInsideRelevantFiles(t *testing.T) {
	summary := fullSummary("- model recalled x.go")
	out := AppendRecorded(summary, []string{"a.go"}, []string{"a.go", "b.go"})
	if !strings.Contains(out, "- model recalled x.go") {
		t.Error("the model-written recollection was disturbed")
	}
	recordedAt := strings.Index(out, "Recorded (tool log, not recollection):")
	decisionsAt := strings.Index(out, "## Decisions")
	modelAt := strings.Index(out, "- model recalled x.go")
	if recordedAt < 0 || !(modelAt < recordedAt && recordedAt < decisionsAt) {
		t.Errorf("recorded block misplaced:\n%s", out)
	}
	if !strings.Contains(out, "- a.go (read, write)") || !strings.Contains(out, "- b.go (write)") {
		t.Errorf("markers wrong:\n%s", out)
	}
	// The appended lines sit inside a section body, so heading validation must
	// still pass afterwards.
	if err := validateSummary(out); err != nil {
		t.Errorf("appended summary fails validation: %v", err)
	}
}

func TestAppendRecordedCapsAndElides(t *testing.T) {
	var reads []string
	for i := 0; i < MaxRecordedFiles+5; i++ {
		reads = append(reads, string(rune('a'+i%26))+string(rune('0'+i%10))+".go")
	}
	summary := fullSummary("None yet.")
	out := AppendRecorded(summary, reads, nil)
	if !strings.Contains(out, "more recorded files elided") {
		t.Errorf("no elision line:\n%s", out)
	}
	if err := validateSummary(out); err != nil {
		t.Errorf("appended summary fails validation: %v", err)
	}
}

func TestAppendRecordedLeavesEmptyAndHeadinglessAlone(t *testing.T) {
	summary := fullSummary("- x.go")
	if out := AppendRecorded(summary, nil, nil); out != summary {
		t.Error("empty sets changed the text")
	}
	other := "## Objective\nGoal.\n"
	if out := AppendRecorded(other, []string{"a.go"}, nil); out != other {
		t.Error("missing heading changed the text rather than passing through")
	}
}

// fullSummary builds a summary carrying every required heading in order, with
// the given body under Relevant Files, so validation-focused tests start from
// a text that passes before the append.
func fullSummary(relevantBody string) string {
	var b strings.Builder
	for _, heading := range requiredSummaryHeadings() {
		b.WriteString("## " + heading + "\n")
		if heading == "Relevant Files" {
			b.WriteString(relevantBody + "\n")
		} else {
			b.WriteString("Carried forward from the folded span for test grounding padding.\n")
		}
	}
	return b.String()
}
