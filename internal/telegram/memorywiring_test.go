package telegram

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ceraa/harness/coordinator"
	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/internal/config"
)

// memoryWiringFixture builds the pieces a run builds: a project with a memory
// file, a store, and the reader for the project's append-only log.
//
// The memory file is written for real, because "MemoryContextFunc still works"
// is a claim about rendered text: a config whose renderer answers with nothing
// would pass a nil check and fail the model.
func memoryWiringFixture(t *testing.T) (projectRoot string, store *memory.Store, log *memory.MemoryFile) {
	t.Helper()
	projectRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectRoot, "memory"), 0o755); err != nil {
		t.Fatalf("mkdir memory dir: %v", err)
	}
	const note = "The gate is the local check, never CI."
	if err := os.WriteFile(filepath.Join(projectRoot, "memory", "AGENTS.md"), []byte(note+"\n"), 0o644); err != nil {
		t.Fatalf("write memory file: %v", err)
	}
	store, err := memory.New(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("memory store: %v", err)
	}
	log = memoryLogReader(projectRoot)
	return projectRoot, store, log
}

// assertNoExtraRenderer walks a constructed RunnerConfig for any live renderer
// that is not the memory context.
func assertNoExtraRenderer(t *testing.T, cfg coordinator.RunnerConfig) {
	t.Helper()
	rendererType := reflect.TypeOf((func() (string, error))(nil))
	value := reflect.ValueOf(cfg)
	typ := value.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type != rendererType || value.Field(i).IsNil() {
			continue
		}
		if field.Name == "MemoryContextFunc" {
			continue
		}
		t.Errorf("RunnerConfig.%s is set: only the memory renderer may reach the prompt", field.Name)
	}
}

// assertRendersMemoryOnly is the contract this wiring has to keep: the memory
// block still renders the project's memory and nothing else.
func assertRendersMemoryOnly(t *testing.T, cfg coordinator.RunnerConfig) {
	t.Helper()
	if cfg.MemoryContextFunc == nil {
		t.Fatal("MemoryContextFunc is nil, so the prompt modes have no memory to read")
	}
	rendered, err := cfg.MemoryContextFunc()
	if err != nil {
		t.Fatalf("MemoryContextFunc: %v", err)
	}
	if !strings.Contains(rendered, "The gate is the local check, never CI.") {
		t.Errorf("memory context does not carry the project's memory:\n%s", rendered)
	}
}

// TestAgentRunnerConfigCarriesMemoryWiring pins the config a Telegram run
// actually receives: the only renderer is the memory one, and it renders memory
// and nothing else.
func TestAgentRunnerConfigCarriesMemoryWiring(t *testing.T) {
	projectRoot, store, log := memoryWiringFixture(t)

	cfg := agentRunnerConfig(agentRunDeps{
		sessionID:     "tg-memory-1",
		cfg:           &config.Config{},
		memoryContext: "startup snapshot",
		memory:        newMemoryWiring(store, projectRoot, "tg-memory-1", log),
		memoryStore:   store,
	})

	assertNoExtraRenderer(t, cfg)
	assertRendersMemoryOnly(t, cfg)
}

// TestAgentRunnerConfigKeepsTheMemoryFileWiring pins the reader for the
// append-only log, on the same config, at the project root.
func TestAgentRunnerConfigKeepsTheMemoryFileWiring(t *testing.T) {
	projectRoot, store, log := memoryWiringFixture(t)

	cfg := agentRunnerConfig(agentRunDeps{
		sessionID:   "tg-memory-2",
		cfg:         &config.Config{},
		memory:      newMemoryWiring(store, projectRoot, "tg-memory-2", log),
		memoryStore: store,
	})

	if cfg.MemoryFile == nil {
		t.Fatal("MemoryFile is nil: the runner would read a log nothing opens")
	}
	reader := cfg.MemoryFile()
	if want := filepath.Join(projectRoot, "Memory.md"); reader.Path() != want {
		t.Fatalf("MemoryFile path = %q, want %q", reader.Path(), want)
	}
	if err := reader.Append("an entry from the config's own reader", "tg-memory-2"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	entries := reader.Tail(10)
	if len(entries) == 0 || !strings.Contains(entries[len(entries)-1], "an entry from the config's own reader") {
		t.Fatalf("Tail = %v, want the entry just appended", entries)
	}
}

// TestAgentLearnChainWritesTheLog is the checkpoint contract for memory-only
// learning: the chain still writes the log.
func TestAgentLearnChainWritesTheLog(t *testing.T) {
	projectRoot, store, log := memoryWiringFixture(t)

	chain := agentLearnChain(llm.ProviderConfig{BaseURL: "http://127.0.0.1:1", Model: "unused"}, store, projectRoot, log)
	if chain == nil {
		t.Fatal("learn chain is nil, so the append-only log would never be written")
	}
	chain(context.Background(), "tg-memory-3", nil, "a checkpoint worth recording")

	entries := log.Tail(10)
	if len(entries) == 0 || !strings.Contains(entries[len(entries)-1], "a checkpoint worth recording") {
		t.Fatalf("Tail = %v, want the summary the compaction produced", entries)
	}
}

// TestAgentLearnChainRunsTheExtractor is the chain with extraction configured:
// the extractor still runs off the checkpoint and the log is still written.
func TestAgentLearnChainRunsTheExtractor(t *testing.T) {
	t.Setenv("CERAA_EXTRACT_KEY", "test-extract-key")
	projectRoot, store, log := memoryWiringFixture(t)

	chain := agentLearnChain(llm.ProviderConfig{BaseURL: "http://127.0.0.1:1", Model: "unused"}, store, projectRoot, log)
	if chain == nil {
		t.Fatal("learn chain is nil with an extraction credential configured, so the extractor is no longer wired")
	}
	chain(context.Background(), "tg-memory-4", nil, "a second checkpoint")

	entries := log.Tail(10)
	if len(entries) == 0 || !strings.Contains(entries[len(entries)-1], "a second checkpoint") {
		t.Fatalf("Tail = %v, want the summary the compaction produced", entries)
	}
}

// TestWebLiveRunnerConfigCarriesMemoryWiring is the dashboard's half of the
// same claim: the config the compaction button previews from carries the memory
// renderer and the log reader, and no second renderer.
func TestWebLiveRunnerConfigCarriesMemoryWiring(t *testing.T) {
	projectRoot, store, log := memoryWiringFixture(t)

	cfg := webLiveRunnerConfig(webLiveDeps{
		sessionID:   "web-memory-1",
		cfg:         &config.Config{},
		projectRoot: projectRoot,
		memory:      newMemoryWiring(store, projectRoot, "web-memory-1", log),
		memoryStore: store,
	})

	assertNoExtraRenderer(t, cfg)
	assertRendersMemoryOnly(t, cfg)
	if cfg.MemoryFile == nil {
		t.Fatal("MemoryFile is nil: the previewed request would read a log nothing opens")
	}
	if want := filepath.Join(projectRoot, "Memory.md"); cfg.MemoryFile().Path() != want {
		t.Fatalf("MemoryFile path = %q, want %q", cfg.MemoryFile().Path(), want)
	}
}
