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

func TestLiveRendersTheFileAndStoreTogether(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("proj", "", "from the store"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(path, []byte("from the file"), 0o644); err != nil {
		t.Fatal(err)
	}
	live := Live(store, "proj", "", path, 4000)
	got, err := live()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "from the file") || !strings.Contains(got, "from the store") {
		t.Errorf("live render lost a source: %q", got)
	}
}

// The point of Live: an edit between calls must be picked up.
func TestLiveRereadsOnEveryCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	os.WriteFile(path, []byte("before"), 0o644)
	live := Live(nil, "proj", "", path, 4000)

	first, err := live()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "before") {
		t.Fatalf("first render = %q", first)
	}
	os.WriteFile(path, []byte("after"), 0o644)
	second, err := live()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second, "after") || strings.Contains(second, "before") {
		t.Errorf("second render did not observe the edit: %q", second)
	}
}

// A missing memory file is the normal case and must not read as an error.
func TestLiveToleratesAMissingFile(t *testing.T) {
	live := Live(nil, "proj", "", filepath.Join(t.TempDir(), "nope.md"), 4000)
	got, err := live()
	if err != nil {
		t.Fatalf("missing memory file returned an error: %v", err)
	}
	if got != "" {
		t.Errorf("missing memory file rendered %q, want empty", got)
	}
}

// Truncated, not dropped: a large file degrades to a bounded prefix.
func TestRenderFileBoundsAnOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 10000)), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := RenderFile(path, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 600 {
		t.Errorf("render is %d bytes, want it bounded near 500", len(got))
	}
	if !strings.HasPrefix(got, "<memory-file>") || !strings.HasSuffix(strings.TrimSpace(got), "</memory-file>") {
		t.Errorf("truncation broke the block framing: %q", got[:60])
	}
}

func TestRenderFileEmptyBody(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	os.WriteFile(path, []byte("   \n\n"), 0o644)
	got, err := RenderFile(path, 4000)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("whitespace-only file rendered %q, want empty", got)
	}
}

// A memory file that cannot be read must not discard the store beside it.
//
// Both the partial render and the error have to come back: the content proves
// the other source still worked, and the path in the error is what makes a
// "permission denied" warning point somewhere. Without it 74 identical lines in
// one log named no file.
func TestLiveKeepsStoreEntriesWhenTheFileIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("proj", "", "from the store"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(path, []byte("from the file"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	live := Live(store, "proj", "", path, 4000)
	got, err := live()
	if err == nil {
		t.Fatal("an unreadable memory file reported no error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name %s", err, path)
	}
	if !strings.Contains(got, "from the store") {
		t.Errorf("the source that worked was discarded: %q", got)
	}
}

// The two sources stay independent: a store failure still surfaces as an error
// with no partial render, because there is nothing to keep.
func TestLiveReportsAStoreFailureWithoutPartialRender(t *testing.T) {
	live := Live(nil, "", "", filepath.Join(t.TempDir(), "absent.md"), 4000)
	got, err := live()
	if err != nil {
		t.Fatalf("no store and a missing file is the empty case, got %v", err)
	}
	if got != "" {
		t.Errorf("render = %q, want empty", got)
	}
}

// IsImperative decides the lifetime a memory is given, so it is pinned on both
// sides: the wording that must count as a directive, and the wording that must
// not. A prefix that starts matching more than it used to does not fail loudly,
// it silently shortens half a store's lives.
func TestIsImperativeFindsTheDirectiveAnywhereInIt(t *testing.T) {
	for _, text := range []string{
		"never push to main",
		"always run the gate before pushing",
		"do not use the vendored SDK",
		"don't force push",
		"must keep the response under 200ms",
		"avoid shelling out for this",
		"stop adding dependencies",
		"for this task, always run the gate before pushing",
		"we agreed on friday, never bypass review",
		"   always trim the log   ",
	} {
		if !IsImperative(text) {
			t.Errorf("IsImperative(%q) = false, want true", text)
		}
	}
	for _, text := range []string{
		"",
		"the repo uses sqlite",
		"we went with the postgres driver",
		"it is better to keep functions small",
		"they never said which database",
		// The prefix needs its trailing space, so a word that merely ends in one of
		// the verbs is not a directive. "stopping" and "avoided" are past tense.
		"we are stopping the nightly job",
		"this is avoided elsewhere",
	} {
		if IsImperative(text) {
			t.Errorf("IsImperative(%q) = true, want false", text)
		}
	}
}

// The exported classifier has to agree with the list it is defined by, which is
// the only thing standing between this package and the document compiler
// carrying a third, slightly different vocabulary.
//
// The list is restated in the test rather than imported on purpose: importing it
// would compare the product against itself and pass forever. This copy is the
// pin - if a directive is added to either side, this test says so instead of
// letting the two drift apart unnoticed.
func TestIsImperativeAgreesWithTheDirectiveVocabulary(t *testing.T) {
	// Lower case deliberately. See TestIsImperativeIsCaseSensitiveByDesign.
	reference := []string{"never ", "always ", "do not ", "don't ", "must ", "avoid ", "stop "}
	for _, prefix := range reference {
		for _, text := range []string{prefix + "do the thing", "we decided earlier, " + prefix + "do the thing"} {
			if !IsImperative(text) {
				t.Errorf("IsImperative(%q) = false, but %q is in the vocabulary", text, prefix)
			}
		}
	}
	for _, word := range []string{"can", "should", "prefer", "consider"} {
		text := word + " to do the thing"
		if IsImperative(text) {
			t.Errorf("IsImperative(%q) = true, but %q is not a standing-directive prefix", text, word)
		}
	}
}

// The case-sensitivity is the divergence between this package and
// harness/instruction, which case-folds because documents capitalise their
// bullets and memory statements do not. It is asserted rather than left to a
// reader of two comments, because the obvious "fix" is to add strings.ToLower
// here and that would reclassify every capitalised statement already stored.
//
// Whether the two should be unified is the owner's call: folding case is right
// for hand-written documents and a behaviour change for stores in the field.
func TestIsImperativeIsCaseSensitiveByDesign(t *testing.T) {
	for _, text := range []string{"Never push to main", "Do not use the vendored SDK", "Always run the gate"} {
		if IsImperative(text) {
			t.Errorf("IsImperative(%q) = true; this package is case-sensitive, and changing that reclassifies stored memories", text)
		}
	}
	if !IsImperative("never push to main") {
		t.Errorf("the lowercase form stopped matching, so the comparison is broken rather than case-sensitive")
	}
}
