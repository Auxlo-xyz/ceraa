package telegram

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/internal/config"
)

// memoryWiringFixture builds the pieces a run builds: a project root and the
// reader for the project's append-only log.
func memoryWiringFixture(t *testing.T) (projectRoot string, log *memory.MemoryFile) {
	t.Helper()
	projectRoot = t.TempDir()
	log = memoryLogReader(projectRoot)
	return projectRoot, log
}

// TestAgentRunnerConfigKeepsTheMemoryFileWiring pins the reader for the
// append-only log, on the config a Telegram run actually receives, at the
// project root.
func TestAgentRunnerConfigKeepsTheMemoryFileWiring(t *testing.T) {
	projectRoot, log := memoryWiringFixture(t)

	cfg := agentRunnerConfig(agentRunDeps{
		sessionID: "tg-memory-2",
		cfg:       &config.Config{},
		memory:    newMemoryWiring(log),
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
	_, log := memoryWiringFixture(t)

	chain := agentLearnChain(llm.ProviderConfig{BaseURL: "http://127.0.0.1:1", Model: "unused"}, log)
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
	_, log := memoryWiringFixture(t)

	chain := agentLearnChain(llm.ProviderConfig{BaseURL: "http://127.0.0.1:1", Model: "unused"}, log)
	if chain == nil {
		t.Fatal("learn chain is nil with an extraction credential configured, so the extractor is no longer wired")
	}
	chain(context.Background(), "tg-memory-4", nil, "a second checkpoint")

	entries := log.Tail(10)
	if len(entries) == 0 || !strings.Contains(entries[len(entries)-1], "a second checkpoint") {
		t.Fatalf("Tail = %v, want the summary the compaction produced", entries)
	}
}

// TestWebLiveRunnerConfigKeepsTheMemoryFileWiring is the dashboard's half of
// the same claim: the config the compaction button previews from carries the
// log reader at the project root.
func TestWebLiveRunnerConfigKeepsTheMemoryFileWiring(t *testing.T) {
	projectRoot, log := memoryWiringFixture(t)

	cfg := webLiveRunnerConfig(webLiveDeps{
		sessionID:   "web-memory-1",
		cfg:         &config.Config{},
		projectRoot: projectRoot,
		memory:      newMemoryWiring(log),
	})

	if cfg.MemoryFile == nil {
		t.Fatal("MemoryFile is nil: the previewed request would read a log nothing opens")
	}
	if want := filepath.Join(projectRoot, "Memory.md"); cfg.MemoryFile().Path() != want {
		t.Fatalf("MemoryFile path = %q, want %q", cfg.MemoryFile().Path(), want)
	}
}
