package snapshot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureAndRevertLatest(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := New(filepath.Join(root, "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Capture(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err := store.RevertLatest(file)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("expected snapshot entry")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "before" {
		t.Fatalf("content = %q", data)
	}
}

func TestRevertMissingFile(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "new.txt")
	store, err := New(filepath.Join(root, "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Capture(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("created"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RevertLatest(file); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("expected file removal, stat error = %v", err)
	}
}
