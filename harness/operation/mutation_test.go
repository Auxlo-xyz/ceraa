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
		mgr.AddSequential(Operation{ID: string(rune('a' + i)), Spec: Spec{Type: "write", Args: map[string]any{"path": path, "content": "value\n"}}})
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
