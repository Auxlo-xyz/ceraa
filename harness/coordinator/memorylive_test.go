package coordinator

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func liveRunner(memoryPath string) *Runner {
	return &Runner{cfg: RunnerConfig{
		SystemPrompt: "STATIC",
		MemoryContextFunc: func() (string, error) {
			raw, err := os.ReadFile(memoryPath)
			if err != nil {
				return "", err
			}
			return "<memory-file>\n" + strings.TrimSpace(string(raw)) + "\n</memory-file>", nil
		},
	}}
}

// The defect: memory was rendered once at construction, so an edit made during a
// session never reached the model and the file on disk silently diverged from the
// memory in the prompt.
func TestMemoryEditIsVisibleWithoutARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(path, []byte("first version"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := liveRunner(path)

	first := r.systemPrompt()
	if !strings.Contains(first, "first version") {
		t.Fatalf("first render missing memory: %q", first)
	}

	// Edit mid-session. No restart, no rebuild, no new Runner.
	if err := os.WriteFile(path, []byte("second version"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := r.systemPrompt()
	if !strings.Contains(second, "second version") {
		t.Errorf("memory edit did not reach the prompt: %q", second)
	}
	if strings.Contains(second, "first version") {
		t.Errorf("stale memory still in the prompt: %q", second)
	}
}

// A failing renderer must degrade to the startup snapshot rather than dropping
// memory, and must be visible in the log rather than looking like empty memory.
func TestMemoryRenderFailureFallsBackAndDoesNotFailTheTurn(t *testing.T) {
	r := &Runner{cfg: RunnerConfig{
		SystemPrompt:      "STATIC",
		MemoryContext:     "<memory>snapshot</memory>",
		MemoryContextFunc: func() (string, error) { return "", os.ErrPermission },
	}}
	prompt := r.systemPrompt()
	if !strings.Contains(prompt, "snapshot") {
		t.Errorf("fallback snapshot missing: %q", prompt)
	}
}

// With no live renderer the startup snapshot is still honoured, so existing
// callers that set only MemoryContext keep working.
func TestStaticMemoryContextStillUsedWhenNoLiveRenderer(t *testing.T) {
	r := &Runner{cfg: RunnerConfig{SystemPrompt: "STATIC", MemoryContext: "<memory>static</memory>"}}
	if !strings.Contains(r.systemPrompt(), "static") {
		t.Error("static memory context was dropped")
	}
}

// Memory must sit after the static prompt and the date, so editing it invalidates
// only the varying suffix rather than the whole cached prefix.
func TestMemorySitsAfterTheStaticPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	os.WriteFile(path, []byte("MEMORYBODY"), 0o644)
	r := liveRunner(path)
	prompt := r.systemPrompt()
	staticAt := strings.Index(prompt, "STATIC")
	memoryAt := strings.Index(prompt, "MEMORYBODY")
	dateAt := strings.Index(prompt, "<current-date>")
	if staticAt < 0 || memoryAt < 0 {
		t.Fatalf("missing sections: static=%d memory=%d", staticAt, memoryAt)
	}
	if memoryAt < staticAt {
		t.Error("memory precedes the static prompt, so a memory edit invalidates the cached prefix")
	}
	if dateAt >= 0 && memoryAt < dateAt {
		t.Error("memory precedes the date, so a daily rollover re-reads memory needlessly")
	}
}

// Concurrency: memory is read per turn, and turns are not serialised.
func TestMemoryRenderIsSafeAcrossConcurrentTurns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	os.WriteFile(path, []byte("concurrent"), 0o644)
	r := liveRunner(path)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !strings.Contains(r.systemPrompt(), "concurrent") {
				t.Error("concurrent render lost memory")
			}
		}()
	}
	wg.Wait()
}
