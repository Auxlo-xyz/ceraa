package bond

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/sessionstore"
)

// isolate opens a log in a temp dir so no test touches a real one.
func isolate(t *testing.T) (*memory.MemoryFile, string) {
	t.Helper()
	dir := t.TempDir()
	return memory.OpenMemoryFile(filepath.Join(dir, "Memory.md")), dir
}

func doc(content string) Document { return Document{Content: content} }

// legacyBond writes a file in the pre-log format and returns its path.
func legacyBond(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "BOND.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// enableExtraction configures a credential so MakeLearner reports enabled.
func enableExtraction(t *testing.T) {
	t.Helper()
	t.Setenv("CERAA_EXTRACT_KEY", "test-key")
}

// settle waits for a detached extraction to finish. The goroutine's last act
// before its deferred unlock is the commit, so holding the guard briefly
// guarantees the entry is on disk and that nothing from this extraction is
// still in flight when the test restores its stubAsk.
func settle(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		running.Lock()
		running.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the detached extraction never released the running guard")
	}
}

// waitForEntries blocks until the log carries n entries, then settles.
func waitForEntries(t *testing.T, log *memory.MemoryFile, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := log.Lines(); got >= n {
			settle(t)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the log holds %d entries, want at least %d", log.Lines(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The document lands as ONE entry. Appending to a log of memories is the whole
// of what a commit is now.
func TestCommitAppendsOneEntryRatherThanRewriting(t *testing.T) {
	log, _ := isolate(t)
	if err := Commit(log, doc("# BOND\n\nName: someone")); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if log.Lines() != 1 {
		t.Fatalf("commit produced %d entries, want 1: the document is one log entry", log.Lines())
	}
	entries := log.Tail(1)
	if len(entries) != 1 || !strings.Contains(entries[0], "Name: someone") {
		t.Fatalf("the entry does not carry the document: %#v", entries)
	}
	if !strings.Contains(entries[0], identityHeading) {
		t.Error("the entry lost the identity heading, so nothing downstream can find it")
	}
}

// Two compactions leave two entries. The versioned store this replaces kept
// ONE current version, so the second commit overwrote the first and the whole
// bond was whatever the last round happened to say. This is the behavioural
// change, and it is the one a regression would silently undo.
func TestTwoCommitsLeaveTwoEntries(t *testing.T) {
	log, _ := isolate(t)
	for _, content := range []string{"# BOND\n\nName: first", "# BOND\n\nName: second"} {
		if err := Commit(log, doc(content)); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	entries := log.Tail(2)
	if len(entries) != 2 {
		t.Fatalf("two commits left %d entries: an append-only log does not overwrite", len(entries))
	}
	if !strings.Contains(entries[0], "first") || !strings.Contains(entries[1], "second") {
		t.Errorf("entries are not in write order: %#v", entries)
	}
	raw, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "first") {
		t.Error("the earlier document was not preserved on disk")
	}
}

// clean still runs at the sink. Extract cleans its own output, but
// Extract's output and Commit's input are not the same call, and a validator
// upstream of the write is a validator a later caller can route around.
func TestCommitRefusesADocumentCleanRejects(t *testing.T) {
	log, _ := isolate(t)
	for _, junk := range []string{"Understood.", "Noted", "", "   \n ", "[]", "```\nsome prose\n```"} {
		if err := Commit(log, doc(junk)); err == nil {
			t.Errorf("Commit accepted %q", junk)
		}
	}
	if log.Lines() != 0 {
		t.Errorf("a refused commit still wrote an entry: %d", log.Lines())
	}
	if err := Commit(nil, doc("# BOND\n\nName: x")); err == nil {
		t.Error("committing to no log at all was accepted")
	}
}

// clean is the fence and preamble guard. "Understood." is the observed failure
// and must be refused, not appended.
func TestCleanRefusesNonBondAnswers(t *testing.T) {
	for _, junk := range []string{"Understood.", "Noted", "", "   \n ", "[]"} {
		if got := clean(junk); got != "" {
			t.Errorf("clean(%q) = %q, want empty", junk, got)
		}
	}
	for _, good := range []string{
		"# BOND\n\n## User\nName: x",
		"```markdown\n# BOND\n\n## Mind\nMood: calm\n```",
	} {
		if got := clean(good); !strings.Contains(got, identityHeading) {
			t.Errorf("clean rejected a real document: %q -> %q", good, got)
		}
	}
	if got := clean("```markdown\n# BOND\n\n## Mind\n```"); strings.Contains(got, "```") {
		t.Errorf("the fence survived: %q", got)
	}
}

// The trailing imperative is what makes the model produce a document. Probing
// found five runs of five returning "Understood." without it, so its presence
// is a behavior, not a style choice.
func TestExtractorPromptCarriesItsActivationSentence(t *testing.T) {
	if !strings.Contains(ExtractorPrompt, "Write the BOND document now, filling every field you can support.") {
		t.Error("the activation sentence is missing: without it the model acknowledges instead of writing")
	}
	for _, section := range []string{"## User", "## Mind", "## Patterns", "## Interaction",
		"## Rules", "## Context", "## Emotional Interpretation", "## Updating", "## Compression"} {
		if !strings.Contains(ExtractorPrompt, section) {
			t.Errorf("section %q missing from the prompt", section)
		}
	}
}

// A context is the checkpoint summary plus the user's own turns, and not the
// agent's tool traffic.
func TestContextFromKeepsUserTurnsAndDropsToolTraffic(t *testing.T) {
	discarded := []sessionstore.Message{
		{Role: llm.RoleUser, Content: "build the worker"},
		{Role: llm.RoleAssistant, Content: "on it"},
		{Role: llm.RoleTool, Content: "ran npm install"},
		{Role: llm.RoleUser, Content: "now deploy it"},
	}
	got := contextFrom("SUMMARY HERE", discarded)
	if !strings.Contains(got, "SUMMARY HERE") {
		t.Error("the checkpoint summary was dropped")
	}
	for _, want := range []string{"USER: build the worker", "USER: now deploy it"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, unwanted := range []string{"ran npm install", "on it"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("agent traffic leaked into the context: %q", unwanted)
		}
	}
}

// With no user turns the summary alone still has to carry the round.
func TestContextFromFallsBackToTheSummaryAlone(t *testing.T) {
	got := contextFrom("ONLY SUMMARY", []sessionstore.Message{
		{Role: llm.RoleTool, Content: "tool noise"},
	})
	if got != "ONLY SUMMARY" {
		t.Errorf("fallback = %q, want the summary alone", got)
	}
}

// The rolling behaviour at the Extract level: no prior skips the merge pass
// entirely rather than merging with an imaginary document.
func TestExtractWithNoPriorRunsOnePass(t *testing.T) {
	calls := 0
	restore := stubAsk(t, func(payload string) (string, error) {
		calls++
		return "# BOND\n\nName: cold-read", nil
	})
	defer restore()

	got, err := Extract(context.Background(), llm.ProviderConfig{}, nil, "USER: hello")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if calls != 1 {
		t.Errorf("a cold read made %d calls, want 1: with no history there is nothing to reconcile", calls)
	}
	if !strings.Contains(got, "cold-read") {
		t.Errorf("first round lost the document: %q", got)
	}
}

// With history the merge pass runs, and it sees the prior documents in the
// order they happened.
func TestExtractWithPriorReconcilesBothPasses(t *testing.T) {
	var seen []string
	restore := stubAsk(t, func(payload string) (string, error) {
		seen = append(seen, payload)
		if len(seen) == 1 {
			return "# BOND\n\nName: fresh", nil
		}
		return "# BOND\n\nName: merged", nil
	})
	defer restore()

	got, err := Extract(context.Background(), llm.ProviderConfig{},
		[]Document{doc("# BOND\n\nName: older"), doc("# BOND\n\nName: newest")},
		"USER: hello")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("made %d calls, want 2 (extract then merge)", len(seen))
	}
	merge := seen[1]
	if !strings.Contains(merge, "older") || !strings.Contains(merge, "newest") {
		t.Errorf("the merge pass did not receive the window: %q", merge)
	}
	if !strings.Contains(merge, "fresh") {
		t.Error("the merge pass did not receive the fresh reading")
	}
	if strings.Index(merge, "older") > strings.Index(merge, "newest") {
		t.Error("the window was handed to the merge out of order")
	}
	if !strings.Contains(got, "merged") {
		t.Errorf("the merged document was not returned: %q", got)
	}
}

// An empty context is not a round. Refusing keeps a compaction that carried
// nothing from appending anything.
func TestExtractRefusesAnEmptyContext(t *testing.T) {
	restore := stubAsk(t, func(payload string) (string, error) {
		t.Fatal("an empty context still reached the model")
		return "", nil
	})
	defer restore()
	if _, err := Extract(context.Background(), llm.ProviderConfig{}, nil, "   "); err == nil {
		t.Fatal("an empty context was accepted")
	}
}

// The window is read back OUT OF THE LOG, which is the whole of how the rolling
// merge survives with no sidecar file. It is a filter and not a slice: at one
// compaction the log receives identity entries, project memories and the
// checkpoint summary, so the last two entries are not the last two documents.
func TestIdentityWindowReadsTheLastTwoIdentityEntriesFromTheLog(t *testing.T) {
	log, _ := isolate(t)
	for _, entry := range []string{
		"project memory about the api",
		"# BOND\n\nName: oldest",
		"a checkpoint summary",
		"# BOND\n\nName: middle",
		"another project memory",
		"# BOND\n\nName: newest",
	} {
		if err := log.Append(entry, "s"); err != nil {
			t.Fatal(err)
		}
	}
	window := identityWindow(log)
	if len(window) != History {
		t.Fatalf("window holds %d documents, want %d", len(window), History)
	}
	// Oldest first, so the merge sees history in the order it happened.
	if !strings.Contains(window[0].Content, "middle") || !strings.Contains(window[1].Content, "newest") {
		t.Errorf("window is not the last two identity entries, oldest first: %#v", window)
	}
	if strings.Contains(window[0].Content, "oldest") {
		t.Error("the third-newest identity entry was kept")
	}
	// The log's own date line is framing, not document.
	for i, d := range window {
		if strings.Contains(d.Content, "## ") {
			t.Errorf("document %d carries the log's date line: %q", i, d.Content)
		}
	}
}

// A log with no identity in it is a cold read, not a broken window. There used
// to be a corrupt-history case here, and it is gone with the file: a log
// nothing can parse is a log identityWindow simply finds nothing in.
func TestIdentityWindowIsEmptyWithoutIdentityEntries(t *testing.T) {
	log, _ := isolate(t)
	if err := log.Append("project memory", "s"); err != nil {
		t.Fatal(err)
	}
	if window := identityWindow(log); len(window) != 0 {
		t.Errorf("a log with no identity entries produced a window of %d", len(window))
	}
	if window := identityWindow(nil); window != nil {
		t.Error("a nil log produced a window")
	}
}

// Nothing writes a history file. The store this replaces wrote BOND.history.json
// beside the document and kept the last two versions there; an append-only log
// is its own history, so there is no second file to keep in step and no
// atomic rewrite to get wrong.
func TestNothingWritesAHistoryFile(t *testing.T) {
	log, dir := isolate(t)
	if err := Commit(log, doc("# BOND\n\nName: someone")); err != nil {
		t.Fatal(err)
	}
	if err := Commit(log, doc("# BOND\n\nName: someone else")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".history") || strings.HasSuffix(entry.Name(), ".json") {
			t.Errorf("a sidecar window was written: %s", entry.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("the log directory holds %d files, want just the log: %v", len(entries), entries)
	}
}

// An install with a BOND.md already on disk keeps its content: one entry, in
// the same log, from the same document.
func TestMigrateCarriesALegacyBondIntoTheLogAsOneEntry(t *testing.T) {
	log, dir := isolate(t)
	legacy := legacyBond(t, dir, "# BOND\n\nName: legacy person\n")
	migrated, err := Migrate(log, legacy)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !migrated {
		t.Fatal("nothing was migrated: an install's identity document was stranded")
	}
	if log.Lines() != 1 {
		t.Fatalf("migration wrote %d entries, want 1", log.Lines())
	}
	entries := log.Tail(1)
	if !strings.Contains(entries[0], "Name: legacy person") {
		t.Errorf("the migrated entry lost the content: %#v", entries)
	}
}

// Migrating twice must not duplicate the document. The log is the record, so
// the log is what answers "has this already happened" - a marker file would be
// a second thing to lose.
func TestMigrateRunsOnceRatherThanDuplicatingTheDocument(t *testing.T) {
	log, dir := isolate(t)
	legacy := legacyBond(t, dir, "# BOND\n\nName: legacy person\n")
	if _, err := Migrate(log, legacy); err != nil {
		t.Fatal(err)
	}
	again, err := Migrate(log, legacy)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if again {
		t.Error("a second migration ran: the document is now in the log twice")
	}
	if log.Lines() != 1 {
		t.Errorf("the log holds %d entries after two migrations", log.Lines())
	}
}

// Nothing to migrate is not an error, and a file that is not a bond is not
// migrated either: a stray note in the data directory must not become an
// identity entry.
func TestMigrateDoesNothingWhenThereIsNothingToCarry(t *testing.T) {
	log, dir := isolate(t)
	for _, c := range []struct{ name, content string }{
		{"empty.md", "   \n"},
		{"note.md", "just a note somebody left"},
		{"fenced.md", "```\nprose without a heading\n```"},
	} {
		path := filepath.Join(dir, c.name)
		if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
			t.Fatal(err)
		}
		if migrated, err := Migrate(log, path); err != nil || migrated {
			t.Errorf("%s migrated %v, %v", c.name, migrated, err)
		}
	}
	if migrated, err := Migrate(log, filepath.Join(dir, "absent.md")); err != nil || migrated {
		t.Errorf("an absent legacy file migrated %v, %v", migrated, err)
	}
	if migrated, err := Migrate(nil, filepath.Join(dir, "note.md")); err != nil || migrated {
		t.Errorf("a migration with no log ran %v, %v", migrated, err)
	}
	if log.Lines() != 0 {
		t.Errorf("a refused migration still wrote an entry: %d", log.Lines())
	}
}

// The learner is opt-in on cost, exactly the way memory extraction is, and it
// refuses to run without a log to append to.
func TestLearnerIsDisabledWithoutACredentialOrALog(t *testing.T) {
	enableExtraction(t)
	log, _ := isolate(t)
	t.Setenv("CERAA_EXTRACT_KEY", "")
	if _, enabled := MakeLearner(llm.ProviderConfig{}, log); enabled {
		t.Error("a learner with no extraction credential reported enabled")
	}
	if _, enabled := MakeLearner(llm.ProviderConfig{}, nil); enabled {
		t.Error("a learner with no log reported enabled")
	}
}

// Two compactions produce two identity entries, driven through the hook rather
// than through Commit: the extractor runs, the document lands in the log, and
// the second compaction appends beside the first instead of replacing it.
//
// It also proves the window travels through the log: the second compaction has
// no in-memory state to read, so the merge pass it runs against the first
// entry is the log being read back.
func TestTheHookAppendsOneIdentityEntryPerCompaction(t *testing.T) {
	enableExtraction(t)
	log, _ := isolate(t)
	var mu sync.Mutex
	var payloads []string
	restore := stubAsk(t, func(payload string) (string, error) {
		mu.Lock()
		payloads = append(payloads, payload)
		mu.Unlock()
		switch {
		case strings.Contains(payload, "FIRST CHECKPOINT"):
			return "# BOND\n\nName: one", nil
		case strings.Contains(payload, "SECOND CHECKPOINT"):
			return "# BOND\n\nName: two", nil
		default:
			return "# BOND\n\nName: merged", nil
		}
	})
	defer restore()

	learn, enabled := MakeLearner(llm.ProviderConfig{BaseURL: "https://example.invalid"}, log)
	if !enabled {
		t.Fatal("the learner reported disabled with an extraction credential configured")
	}
	msgs := []sessionstore.Message{{Role: llm.RoleUser, Content: "hi"}}
	learn(context.Background(), "s1", msgs, "FIRST CHECKPOINT")
	waitForEntries(t, log, 1)
	learn(context.Background(), "s1", msgs, "SECOND CHECKPOINT")
	waitForEntries(t, log, 2)

	entries := log.Tail(2)
	if !strings.Contains(entries[0], "Name: one") {
		t.Errorf("the first compaction's document is not the first entry: %#v", entries)
	}
	if !strings.Contains(entries[1], "Name: merged") {
		t.Errorf("the second compaction did not reconcile against the log's first entry: %#v", entries)
	}

	mu.Lock()
	defer mu.Unlock()
	sawWindow := false
	for _, payload := range payloads {
		if strings.Contains(payload, "DOCUMENT 1:") && strings.Contains(payload, "Name: one") {
			sawWindow = true
		}
	}
	if !sawWindow {
		t.Error("the merge pass never received the identity entry already in the log")
	}
}

// The hook must not block the run that triggered the compaction. Learn is
// called inline by the compactor, so a synchronous two-pass extraction would
// add two full model round trips to a turn the user is waiting on.
func TestHookReturnsBeforeExtractionFinishes(t *testing.T) {
	enableExtraction(t)
	log, _ := isolate(t)
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	restore := stubAsk(t, func(payload string) (string, error) {
		once.Do(func() { close(started) })
		<-release
		return "# BOND\n\nName: slow", nil
	})
	defer restore()

	learn, enabled := MakeLearner(llm.ProviderConfig{BaseURL: "https://example.invalid"}, log)
	if !enabled {
		t.Fatal("the learner reported disabled with an extraction credential configured")
	}

	done := make(chan struct{})
	go func() {
		learn(context.Background(), "s", []sessionstore.Message{{Role: llm.RoleUser, Content: "hi"}}, "SUMMARY")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the hook blocked on the model")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the extraction never started")
	}
	close(release)
	settle(t)
}

// Two extractions must not interleave: each reads the window, so a race would
// let the second merge against a document the first has not written yet.
func TestRunningGuardSerialisesExtractions(t *testing.T) {
	var concurrent, peak int
	var mu sync.Mutex
	restore := stubAsk(t, func(payload string) (string, error) {
		mu.Lock()
		concurrent++
		if concurrent > peak {
			peak = concurrent
		}
		mu.Unlock()
		time.Sleep(60 * time.Millisecond)
		mu.Lock()
		concurrent--
		mu.Unlock()
		return "# BOND\n\nName: x", nil
	})
	defer restore()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			running.Lock()
			defer running.Unlock()
			_, _ = Extract(context.Background(), llm.ProviderConfig{}, nil, "USER: hi")
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if peak > 1 {
		t.Errorf("%d extractions ran concurrently, want 1", peak)
	}
}

// The entry records which session produced it, because the log is read by a
// later run that was not there.
func TestTheEntryCarriesItsSession(t *testing.T) {
	log, _ := isolate(t)
	if err := Commit(log, Document{Session: "sess-42", Content: "# BOND\n\nName: someone"}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	entries := log.Tail(1)
	if !strings.Contains(entries[0], "sess-42") {
		t.Errorf("the entry does not name its session: %q", entries[0])
	}
}
