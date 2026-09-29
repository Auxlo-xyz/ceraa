package coordinator

import (
	"testing"

	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
)

func TestNoteFileMutationReportsACompletedWrite(t *testing.T) {
	var seen []string
	r := &Runner{cfg: RunnerConfig{OnFileMutated: func(p string) { seen = append(seen, p) }}}
	sess := &sessionstore.Session{Operations: []sessionstore.Operation{
		{ID: "op1", SpecType: "write", SpecArgs: map[string]any{"path": "/repo/a.ts"}},
		{ID: "op2", SpecType: "edit_line", SpecArgs: map[string]any{"path": "/repo/b.ts"}},
		{ID: "op3", SpecType: "batch_edit_file", SpecArgs: map[string]any{"path": "/repo/c.ts"}},
		{ID: "op4", SpecType: "read", SpecArgs: map[string]any{"path": "/repo/d.ts"}},
		{ID: "op5", SpecType: "shell", SpecArgs: map[string]any{"command": "touch e"}},
	}}
	for _, id := range []string{"op1", "op2", "op3", "op4", "op5"} {
		r.noteFileMutation(sess, operation.ResultEvent{OpID: id, Status: operation.StatusCompleted})
	}
	if len(seen) != 3 {
		t.Fatalf("reported %v, want the three file mutations only", seen)
	}
	for _, want := range []string{"/repo/a.ts", "/repo/b.ts", "/repo/c.ts"} {
		found := false
		for _, got := range seen {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was not reported", want)
		}
	}
}

// A failed write leaves the old contents on disk. Indexing them would put a file
// into the index that the agent believes it has changed.
func TestNoteFileMutationSkipsAFailedWrite(t *testing.T) {
	var seen []string
	r := &Runner{cfg: RunnerConfig{OnFileMutated: func(p string) { seen = append(seen, p) }}}
	sess := &sessionstore.Session{Operations: []sessionstore.Operation{
		{ID: "op1", SpecType: "write", SpecArgs: map[string]any{"path": "/repo/a.ts"}},
	}}
	r.noteFileMutation(sess, operation.ResultEvent{OpID: "op1", Status: operation.StatusFailed})
	if len(seen) != 0 {
		t.Errorf("a failed write was reported: %v", seen)
	}
}

func TestNoteFileMutationIgnoresAnEmptyPath(t *testing.T) {
	var seen []string
	r := &Runner{cfg: RunnerConfig{OnFileMutated: func(p string) { seen = append(seen, p) }}}
	sess := &sessionstore.Session{Operations: []sessionstore.Operation{
		{ID: "op1", SpecType: "write", SpecArgs: map[string]any{"path": "   "}},
		{ID: "op2", SpecType: "write", SpecArgs: map[string]any{}},
	}}
	r.noteFileMutation(sess, operation.ResultEvent{OpID: "op1", Status: operation.StatusCompleted})
	r.noteFileMutation(sess, operation.ResultEvent{OpID: "op2", Status: operation.StatusCompleted})
	if len(seen) != 0 {
		t.Errorf("an empty path was reported: %v", seen)
	}
}

func TestNoteFileMutationIsSafeWithoutTheHook(t *testing.T) {
	r := &Runner{}
	sess := &sessionstore.Session{Operations: []sessionstore.Operation{
		{ID: "op1", SpecType: "write", SpecArgs: map[string]any{"path": "/repo/a.ts"}},
	}}
	r.noteFileMutation(sess, operation.ResultEvent{OpID: "op1", Status: operation.StatusCompleted})
}
