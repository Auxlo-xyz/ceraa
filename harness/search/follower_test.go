package search

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestFollowerIndexesAWriteItIsToldAbout(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	target := filepath.Join(root, "types.ts")
	if err := os.WriteFile(target, []byte("export interface Battlecard { id: string }"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := NewFollower(filepath.Join(dir, "index.db"), root)

	f.Note(target)
	f.FlushNow()

	idx, err := Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("the index was not created by following a write: %v", err)
	}
	defer func() { _ = idx.Close() }()
	hits, err := idx.Search("Battlecard", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("the written file is not in the index, so a search would return nothing")
	}
}

// The whole point of a debounce: a turn that rewrites one file in several steps
// should cost one index update, and the content indexed must be the final one.
func TestFollowerCoalescesAndKeepsTheLastContent(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	target := filepath.Join(root, "a.ts")
	if err := os.WriteFile(target, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := NewFollower(filepath.Join(dir, "index.db"), root)

	var mu sync.Mutex
	var indexed []string
	f.indexed = func(path string, err error) {
		if err == nil {
			mu.Lock()
			indexed = append(indexed, path)
			mu.Unlock()
		}
	}
	for _, content := range []string{"first", "second", "third"} {
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		f.Note(target)
	}
	if got := f.Pending(); got != 1 {
		t.Errorf("%d paths queued, want 1; three writes to one file should coalesce", got)
	}
	f.FlushNow()
	mu.Lock()
	got := len(indexed)
	mu.Unlock()
	if got != 1 {
		t.Errorf("%d index updates for three writes to one file, want 1", got)
	}

	idx, err := Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	if _, err := idx.Search("third", 10); err != nil {
		t.Fatal(err)
	} else if hits, _ := idx.Search("third", 10); len(hits) == 0 {
		t.Error("the final content is not what got indexed")
	}
	if hits, _ := idx.Search("first", 10); len(hits) > 0 {
		t.Error("stale content from an earlier write survived")
	}
}

// An index that follows a write should not follow writes outside its tree, or a
// temporary file or an unrelated path pulls outside content into the index.
func TestFollowerIgnoresPathsOutsideItsRoot(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	outside := filepath.Join(elsewhere, "secret.ts")
	if err := os.WriteFile(outside, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := NewFollower(filepath.Join(t.TempDir(), "index.db"), root)
	f.Note(outside)
	if got := f.Pending(); got != 0 {
		t.Errorf("%d out-of-scope paths queued, want 0", got)
	}
}

// A file removed between the write and the flush must leave the index, not
// linger as a path that no longer resolves.
func TestFollowerDropsAFileThatVanished(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	target := filepath.Join(root, "gone.ts")
	if err := os.WriteFile(target, []byte("here"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := NewFollower(filepath.Join(dir, "index.db"), root)
	f.Note(target)
	f.FlushNow()
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	f.Note(target)
	f.FlushNow()

	idx, err := Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	if hits, _ := idx.Search("here", 10); len(hits) > 0 {
		t.Error("a removed file is still in the index")
	}
}

func TestFollowerCloseDrainsWhatIsQueued(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	target := filepath.Join(root, "b.ts")
	if err := os.WriteFile(target, []byte("drained"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := NewFollower(filepath.Join(dir, "index.db"), root)
	f.Note(target)
	f.Close()
	if got := f.Pending(); got != 0 {
		t.Errorf("%d paths still queued after Close", got)
	}
	idx, err := Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	if hits, _ := idx.Search("drained", 10); len(hits) == 0 {
		t.Error("Close dropped a queued write")
	}
	// After Close, nothing new is accepted.
	f.Note(target)
	if got := f.Pending(); got != 0 {
		t.Errorf("%d paths accepted after Close", got)
	}
}

func TestFollowerNilIsSafe(t *testing.T) {
	var f *Follower
	f.Note("x")
	f.FlushNow()
	f.Close()
	if f.Pending() != 0 {
		t.Error("a nil follower reported pending work")
	}
	_ = time.Second
}
