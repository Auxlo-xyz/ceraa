package operation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runEdit(t *testing.T, specType string, args map[string]any) ResultEvent {
	t.Helper()
	mgr := New(nil)
	mgr.Add(Operation{ID: "op", Spec: Spec{Type: specType, Args: args}, Status: StatusPending})
	select {
	case r := <-mgr.Results():
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("timeout")
		return ResultEvent{}
	}
}

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A repeat match used to be a dead end: the error said how many and not where, so
// the only route to a fix was a read round trip or a guess at more context.
func TestAMultiMatchNamesEveryLine(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "f.txt", "alpha\nx\nbeta\nx\ngamma\nx\n")

	result := runEdit(t, "edit_line", map[string]any{
		"path": path, "old_text": "x", "new_text": "y",
	})
	if result.Status != StatusFailed {
		t.Fatalf("a non-unique old_text reported %s", result.Status)
	}
	for _, line := range []string{"2", "4", "6"} {
		if !strings.Contains(result.Error, line) {
			t.Errorf("error = %q, want the line %s named", result.Error, line)
		}
	}
	if !strings.Contains(result.Error, "occurrence") {
		t.Errorf("error = %q, want it to say how to proceed", result.Error)
	}
}

// The point of the above: the model can now finish in one retry, with no read.
func TestOccurrencePicksWhichMatch(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "f.txt", "one\nX\ntwo\nX\nthree\nX\n")

	result := runEdit(t, "edit_line", map[string]any{
		"path": path, "old_text": "X", "new_text": "Y", "occurrence": 2,
	})
	if result.Status != StatusCompleted {
		t.Fatalf("occurrence 2 reported %s: %s", result.Status, result.Error)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "one\nX\ntwo\nY\nthree\nX\n" {
		t.Errorf("the wrong occurrence was replaced:\n%q", content)
	}
}

// The safety property the tool exists for has to survive the new parameter: the
// text must still be present, and a stale read must still fail loudly.
func TestOccurrenceStillRefusesWhenTheTextIsGone(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "f.txt", "alpha\n")

	result := runEdit(t, "edit_line", map[string]any{
		"path": path, "old_text": "not-there", "new_text": "x", "occurrence": 1,
	})
	if result.Status != StatusFailed {
		t.Fatalf("a stale read with an occurrence reported %s", result.Status)
	}
	content, _ := os.ReadFile(path)
	if string(content) != "alpha\n" {
		t.Errorf("a stale read wrote to the file: %q", content)
	}
}

func TestOccurrenceBeyondTheCountIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "f.txt", "X\nX\n")
	result := runEdit(t, "edit_line", map[string]any{
		"path": path, "old_text": "X", "new_text": "Y", "occurrence": 5,
	})
	if result.Status != StatusFailed {
		t.Fatalf("an out-of-range occurrence reported %s", result.Status)
	}
	if !strings.Contains(result.Error, "2") {
		t.Errorf("error = %q, want it to say how many there are", result.Error)
	}
}

// The most common silent failure: the text is there, differing only in
// indentation or a trailing space. "found 0 occurrences" gives the model nothing.
func TestAWhitespaceOnlyDifferenceIsDiagnosed(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "f.txt", "alpha\n\tindented\nbeta\n")

	result := runEdit(t, "edit_line", map[string]any{
		"path": path, "old_text": "    indented", "new_text": "x",
	})
	if result.Status != StatusFailed {
		t.Fatal("a whitespace-different match reported success")
	}
	if !strings.Contains(result.Error, "whitespace") || !strings.Contains(result.Error, "2") {
		t.Errorf("error = %q, want it to name the whitespace difference and the line", result.Error)
	}
}

func TestANoMatchWithNoNeighbourSaysSo(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "f.txt", "alpha\nbeta\n")
	result := runEdit(t, "edit_line", map[string]any{
		"path": path, "old_text": "gamma", "new_text": "x",
	})
	if !strings.Contains(result.Error, "not found") {
		t.Errorf("error = %q", result.Error)
	}
	if strings.Contains(result.Error, "whitespace") {
		t.Errorf("error = %q, must not claim a whitespace difference that does not exist", result.Error)
	}
}

// A batch that stops at the first bad edit costs a round trip per bad edit, which
// is the thing a batch exists to avoid. Every failure has to arrive at once.
func TestABatchReportsEveryFailureAtOnce(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "f.txt", "alpha\nbeta\nX\ngamma\nX\n")

	result := runEdit(t, "batch_edit_file", map[string]any{
		"path": path,
		"edits": []map[string]any{
			{"old_text": "alpha", "new_text": "A"},
			{"old_text": "nope", "new_text": "N"},
			{"old_text": "gamma", "new_text": "G"},
			{"old_text": "X", "new_text": "Q"},
		},
	})
	if result.Status != StatusFailed {
		t.Fatalf("a set with two bad edits reported %s", result.Status)
	}
	if !strings.Contains(result.Error, "2 of the 4 edits") {
		t.Errorf("error = %q, want it to say two of the four failed", result.Error)
	}
	for _, marker := range []string{"edit 1", "edit 3"} {
		if !strings.Contains(result.Error, marker) {
			t.Errorf("error = %q, want it to name %s", result.Error, marker)
		}
	}
	// Edit 3 is a genuine repeat, so its two lines have to be named: that is what
	// turns the failure into a retry the model can write without reading the file.
	if !strings.Contains(result.Error, "3") || !strings.Contains(result.Error, "5") {
		t.Errorf("error = %q, want the repeated match's lines named", result.Error)
	}

	content, _ := os.ReadFile(path)
	if string(content) != "alpha\nbeta\nX\ngamma\nX\n" {
		t.Errorf("a failed batch wrote to the file: %q", content)
	}
}

// A batch edit can name its own occurrence, so a repeated line is reachable
// without a whole extra round trip.
func TestABatchEditCanNameAnOccurrence(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "f.txt", "X\nkeep\nX\ntag\nX\n")

	result := runEdit(t, "batch_edit_file", map[string]any{
		"path": path,
		"edits": []map[string]any{
			{"old_text": "keep", "new_text": "kept"},
			{"old_text": "tag", "new_text": "tagged"},
			{"old_text": "X", "new_text": "Y", "occurrence": 3},
		},
	})
	if result.Status != StatusCompleted {
		t.Fatalf("a batch naming an occurrence reported %s: %s", result.Status, result.Error)
	}
	content, _ := os.ReadFile(path)
	if string(content) != "X\nkept\nX\ntagged\nY\n" {
		t.Errorf("batch applied wrongly:\n%q", content)
	}
}

// Two edits touching the same region must not interfere, or the second is
// resolved against the first's output and the set is not the one that was asked
// for.
func TestOverlappingEditsAreResolvedAgainstTheOriginal(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "f.txt", "header\nbody\nfooter\n")

	result := runEdit(t, "batch_edit_file", map[string]any{
		"path": path,
		"edits": []map[string]any{
			{"old_text": "header", "new_text": "HEADER-LONGER"},
			{"old_text": "footer", "new_text": "FOOTER"},
		},
	})
	if result.Status != StatusCompleted {
		t.Fatalf("adjacent edits reported %s: %s", result.Status, result.Error)
	}
	content, _ := os.ReadFile(path)
	if string(content) != "HEADER-LONGER\nbody\nFOOTER\n" {
		t.Errorf("content = %q", content)
	}
}

// A missing file produced "old_text not found" for every edit, which reads as a
// statement about the text when the real problem is the file.
func TestABatchOnAMissingFileSaysSo(t *testing.T) {
	dir := t.TempDir()
	result := runEdit(t, "batch_edit_file", map[string]any{
		"path": filepath.Join(dir, "nope.txt"),
		"edits": []map[string]any{
			{"old_text": "alpha", "new_text": "A"},
		},
	})
	if result.Status != StatusFailed {
		t.Fatalf("a missing file reported %s", result.Status)
	}
	if strings.Contains(result.Error, "old_text not found") {
		t.Errorf("error = %q, want the read failure, not a claim about the text", result.Error)
	}
	if !strings.Contains(result.Error, "nope.txt") {
		t.Errorf("error = %q, want it to name the file", result.Error)
	}
}
