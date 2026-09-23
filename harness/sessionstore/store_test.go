package sessionstore

import (
	"os"
	"path/filepath"
	"testing"

	"sooraa/harness/llm"
)

func TestSaveIsAtomicAndReloadable(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{ID: "atomic", Messages: []Message{{Role: llm.RoleUser, Content: "hello"}}}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("atomic")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].Content != "hello" {
		t.Fatalf("unexpected session: %+v", loaded)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary session files remain: %v", matches)
	}
	if _, err := os.Stat(filepath.Join(dir, "atomic.json")); err != nil {
		t.Fatal(err)
	}
}

func TestUpdatePersistsClear(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update("clear", func(sess *Session) error {
		sess.Messages = []Message{{Role: llm.RoleUser, Content: "old"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Clear("clear"); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("clear")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 0 {
		t.Fatalf("expected cleared session, got %+v", loaded.Messages)
	}
}
