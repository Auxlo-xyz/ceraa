package memory

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestStorePersistsScopedEntriesAndDeduplicates(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Add("project-a", "", "Prefer   gofmt")
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := store.Add("project-a", "", "Prefer gofmt")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.ID != project.ID || !duplicate.CreatedAt.Equal(project.CreatedAt) {
		t.Fatalf("duplicate created a new entry: first=%+v duplicate=%+v", project, duplicate)
	}
	if project.Scope != Project || project.Content != "Prefer gofmt" {
		t.Fatalf("unexpected project memory: %+v", project)
	}
	session, err := store.Add("project-a", "session-a", "Tests use t.TempDir")
	if err != nil {
		t.Fatal(err)
	}
	if session.Scope != Session {
		t.Fatalf("unexpected session scope: %+v", session)
	}
	if _, err := store.Add("project-b", "session-a", "Not in project A"); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := reopened.List("project-a", "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].ID != project.ID || entries[1].ID != session.ID {
		t.Fatalf("unexpected durable entries: %+v", entries)
	}
	otherSession, err := reopened.List("project-a", "session-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(otherSession) != 1 || otherSession[0].ID != project.ID {
		t.Fatalf("project memory was not shared across sessions: %+v", otherSession)
	}
	if err := reopened.Remove(project.ID); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Remove(project.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected missing entry error, got %v", err)
	}
}

func TestRenderContextIsScopedAndByteBounded(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("project-a", "", "project note"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("project-a", "session-a", "session <note>"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("project-a", "session-b", "private note"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("project-b", "session-a", "other project"); err != nil {
		t.Fatal(err)
	}
	context, err := store.RenderContext("project-a", "session-a", 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(context) > 400 || !utf8.ValidString(context) {
		t.Fatalf("context is not byte bounded: bytes=%d context=%q", len(context), context)
	}
	for _, expected := range []string{"project note", "session &lt;note&gt;"} {
		if !strings.Contains(context, expected) {
			t.Fatalf("context does not contain %q: %q", expected, context)
		}
	}
	for _, excluded := range []string{"private note", "other project"} {
		if strings.Contains(context, excluded) {
			t.Fatalf("context contains %q: %q", excluded, context)
		}
	}
	short, err := store.RenderContext("project-a", "session-a", 37)
	if err != nil {
		t.Fatal(err)
	}
	if len(short) > 37 || !utf8.ValidString(short) {
		t.Fatalf("short context is not byte bounded: bytes=%d context=%q", len(short), short)
	}
}

func TestConcurrentDuplicateAddLeavesOneEntry(t *testing.T) {
	dir := t.TempDir()
	stores := make([]*Store, 6)
	for i := range stores {
		store, err := New(dir)
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = store
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(stores))
	for _, store := range stores {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			_, err := store.Add("project", "session", "same memory")
			errs <- err
		}(store)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, err := stores[0].List("project", "session")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one deduplicated entry, got %d", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(dir, "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatalf("memory store is not valid JSON: %q", data)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".memory.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary memory files remain: %v", matches)
	}
}
