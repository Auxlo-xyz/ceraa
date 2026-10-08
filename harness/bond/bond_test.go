package bond

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

// isolate points the store at a temp dir so no test touches a real bond.
func isolate(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "BOND.md")
}

func doc(content string) Document { return Document{Content: content} }

// The window is the whole rolling design: the first round sees nothing, the
// second sees one document, the third sees two, and it never grows past two.
func TestWindowKeepsExactlyTheLastTwoDocuments(t *testing.T) {
	path := isolate(t)
	st := NewStore(path)
	if recent, err := st.Recent(); err != nil || len(recent) != 0 {
		t.Fatalf("first round saw history: %v, %v", recent, err)
	}
	for _, content := range []string{"# BOND\n\none", "# BOND\n\ntwo", "# BOND\n\nthree"} {
		if err := st.Push(doc(content)); err != nil {
			t.Fatalf("push: %v", err)
		}
	}
	recent, err := st.Recent()
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(recent) != History {
		t.Fatalf("window holds %d documents, want %d", len(recent), History)
	}
	// Oldest first, so the merge sees history in the order it happened.
	if !strings.Contains(recent[0].Content, "two") || !strings.Contains(recent[1].Content, "three") {
		t.Errorf("window is not the last two, oldest first: %q / %q", recent[0].Content, recent[1].Content)
	}
	if strings.Contains(recent[0].Content, "one") {
		t.Error("the oldest document was not evicted")
	}
}

// A corrupt window must not stop the bond being updated. Refusing here would
// make a truncated file permanent.
func TestCorruptWindowReadsAsEmptyRatherThanFailing(t *testing.T) {
	path := isolate(t)
	st := NewStore(path)
	if err := os.WriteFile(st.path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	recent, err := st.Recent()
	if err != nil {
		t.Fatalf("a corrupt window errored instead of reading empty: %v", err)
	}
	if len(recent) != 0 {
		t.Errorf("a corrupt window returned %d documents", len(recent))
	}
}

// The commit writes the file the prompt reads AND advances the window. Miss
// either and the bond is either invisible or frozen.
func TestCommitWritesFileAndAdvancesWindow(t *testing.T) {
	path := isolate(t)
	st := NewStore(path)
	if err := Commit(path, st, doc("# BOND\n\nName: someone")); err != nil {
		t.Fatalf("commit: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("bond file missing after commit: %v", err)
	}
	if !strings.Contains(string(raw), "Name: someone") {
		t.Errorf("bond file does not carry the document: %q", raw)
	}
	recent, _ := st.Recent()
	if len(recent) != 1 {
		t.Errorf("window did not advance on commit: %d documents", len(recent))
	}
}

// An empty document is refused. Writing nothing over a working bond is the
// failure this guards, and it is exactly what a model that acknowledges the
// prompt instead of answering it produces.
func TestCommitRefusesAnEmptyDocument(t *testing.T) {
	path := isolate(t)
	st := NewStore(path)
	if err := Commit(path, st, doc("   ")); err == nil {
		t.Fatal("an empty document was committed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a refused commit still wrote the bond file")
	}
}

// clean is the fence and preamble guard. "Understood." is the observed failure
// and must be refused, not committed over a good bond.
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
		if got := clean(good); !strings.Contains(got, "# BOND") {
			t.Errorf("clean rejected a real bond: %q -> %q", good, got)
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

// An empty context is not a round. Refusing keeps the window from advancing on
// a compaction that carried nothing.
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

// The window file is a list, and a reload reads it back intact.
func TestWindowSurvivesAProcessRestart(t *testing.T) {
	path := isolate(t)
	if err := NewStore(path).Push(doc("# BOND\n\nName: persisted")); err != nil {
		t.Fatal(err)
	}
	recent, err := NewStore(path).Recent()
	if err != nil {
		t.Fatalf("recent after reload: %v", err)
	}
	if len(recent) != 1 || !strings.Contains(recent[0].Content, "persisted") {
		t.Errorf("the window did not survive: %#v", recent)
	}
	raw, _ := os.ReadFile(NewStore(path).path)
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Errorf("the window is not readable JSON: %v", err)
	}
}

// The hook must not block the run that triggered the compaction. Learn is
// called inline by the compactor, so a synchronous two-pass extraction would
// add two full model round trips to a turn the user is waiting on. This
// asserts the hook has returned while the extraction is still running.
func TestHookReturnsBeforeExtractionFinishes(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	restore := stubAsk(t, func(payload string) (string, error) {
		once.Do(func() { close(started) })
		<-release
		return "# BOND\n\nName: slow", nil
	})
	defer restore()

	path := isolate(t)
	st := NewStore(path)
	hook := func(ctx context.Context, sessionID string, d []sessionstore.Message, summary string) {
		one := contextFrom(summary, d)
		prior, _ := st.Recent()
		go func() {
			running.Lock()
			defer running.Unlock()
			content, err := Extract(ctx, llm.ProviderConfig{}, prior, one)
			if err == nil && content != "" {
				_ = Commit(path, st, Document{Content: content})
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		hook(context.Background(), "s", []sessionstore.Message{{Role: llm.RoleUser, Content: "hi"}}, "SUMMARY")
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
	// Wait for the detached extraction to finish before returning. It holds the
	// package-level running guard, and a test that returned while it was still
	// holding it left the next test blocked on a lock nobody would release.
	finished := make(chan struct{})
	go func() {
		running.Lock()
		running.Unlock()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the detached extraction never released the running guard")
	}
}

// Two extractions must not interleave writes: each reads the window, so a
// race would let the second commit discard the first document.
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
