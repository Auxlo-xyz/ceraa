package operation

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEditOperationRequiresUniqueMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := New(nil)
	mgr.Add(Operation{ID: "edit", Spec: Spec{Type: "edit", Args: map[string]any{"path": path, "old_text": "one", "new_text": "three"}}})
	select {
	case result := <-mgr.Results():
		if result.Status != StatusCompleted || result.Output != "edited" {
			t.Fatalf("unexpected edit result: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("edit timeout")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "three\ntwo\n" {
		t.Fatalf("edited content=%q err=%v", data, err)
	}
}

func TestConcurrentWritesSerializeSamePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := New(nil)
	mgr.SetConcurrencyLimit(4)
	for i := 0; i < 8; i++ {
		mgr.AddSequential(Operation{ID: string(rune('a' + i)), Spec: Spec{Type: "write", Args: map[string]any{"path": path, "content": "value\n"}}}, "file")
	}
	for i := 0; i < 8; i++ {
		select {
		case result := <-mgr.Results():
			if result.Status != StatusCompleted {
				t.Fatalf("write result=%+v", result)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("write timeout")
		}
	}
}

// The line counts on a finished mutation are read off the run trail, so they
// have to be right for the real operations and not only for the helper. These
// drive the manager the way a turn does and assert on ResultEvent.
func TestMutationsReportTheirLineCounts(t *testing.T) {
	t.Run("edit replaces lines", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mgr := New(nil)
		mgr.Add(Operation{ID: "e", Spec: Spec{Type: "edit",
			Args: map[string]any{"path": path, "old_text": "one", "new_text": "ONE"}}})
		result := waitForResult(t, mgr)
		if result.Status != StatusCompleted {
			t.Fatalf("status = %v, err = %v", result.Status, result.Error)
		}
		// One line changed, so one out and one in.
		if result.LinesAdded != 1 || result.LinesRemoved != 1 {
			t.Errorf("edit counts = +%d -%d, want +1 -1", result.LinesAdded, result.LinesRemoved)
		}
	})

	t.Run("edit growing a file is additions only", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mgr := New(nil)
		mgr.Add(Operation{ID: "e", Spec: Spec{Type: "edit",
			Args: map[string]any{"path": path, "old_text": "two", "new_text": "two\na\nb\nc"}}})
		result := waitForResult(t, mgr)
		if result.LinesAdded != 3 || result.LinesRemoved != 0 {
			t.Errorf("edit counts = +%d -%d, want +3 -0", result.LinesAdded, result.LinesRemoved)
		}
	})

	t.Run("creating a file counts every line", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "new.txt")
		mgr := New(nil)
		mgr.Add(Operation{ID: "w", Spec: Spec{Type: "write",
			Args: map[string]any{"path": path, "content": "a\nb\nc\nd\n"}}})
		result := waitForResult(t, mgr)
		if result.LinesAdded != 4 || result.LinesRemoved != 0 {
			t.Errorf("create counts = +%d -%d, want +4 -0", result.LinesAdded, result.LinesRemoved)
		}
	})

	t.Run("rewriting reports the difference not the file size", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(path, []byte("a\nb\nc\nd\ne\nf\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mgr := New(nil)
		mgr.Add(Operation{ID: "w", Spec: Spec{Type: "write",
			Args: map[string]any{"path": path, "content": "a\nb\nc\nd\ne\nf\ng\nh\n"}}})
		result := waitForResult(t, mgr)
		// The whole file was written, but only two lines are new. This is the
		// case a naive length comparison gets wrong.
		if result.LinesAdded != 2 || result.LinesRemoved != 0 {
			t.Errorf("rewrite counts = +%d -%d, want +2 -0", result.LinesAdded, result.LinesRemoved)
		}
	})

	t.Run("batch edits are counted together", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(path, []byte("1\n2\n3\n4\n5\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mgr := New(nil)
		mgr.Add(Operation{ID: "b", Spec: Spec{Type: "batch_edit_file",
			Args: map[string]any{"path": path, "edits": []any{
				map[string]any{"old_text": "1", "new_text": "ONE"},
				map[string]any{"old_text": "5", "new_text": "FIVE"},
			}}}})
		result := waitForResult(t, mgr)
		if result.LinesAdded != 2 || result.LinesRemoved != 2 {
			t.Errorf("batch counts = +%d -%d, want +2 -2", result.LinesAdded, result.LinesRemoved)
		}
	})

	t.Run("a failed edit reports nothing", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mgr := New(nil)
		mgr.Add(Operation{ID: "e", Spec: Spec{Type: "edit",
			Args: map[string]any{"path": path, "old_text": "absent", "new_text": "x"}}})
		result := waitForResult(t, mgr)
		if result.Status != StatusFailed {
			t.Fatalf("status = %v, want failed", result.Status)
		}
		// A change that did not happen must not put a number on the trail.
		if result.LinesAdded != 0 || result.LinesRemoved != 0 {
			t.Errorf("failed edit counts = +%d -%d, want 0 0",
				result.LinesAdded, result.LinesRemoved)
		}
	})
}

func waitForResult(t *testing.T, mgr *LocalManager) ResultEvent {
	t.Helper()
	select {
	case result := <-mgr.Results():
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("operation timeout")
		return ResultEvent{}
	}
}
