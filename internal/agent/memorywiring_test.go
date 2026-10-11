package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/internal/config"
)

// memoryWiringFixture builds the pieces a CLI run builds: a project root and
// the reader for the root's append-only log.
func memoryWiringFixture(t *testing.T) (projectRoot string, log *memory.MemoryFile) {
	t.Helper()
	projectRoot = t.TempDir()
	log = memoryLogReader(projectRoot)
	return projectRoot, log
}

// TestAgentRunnerConfigKeepsTheMemoryFileWiring pins the reader for the
// append-only log, on the config a CLI run actually receives, at the project
// root the wiring was built against.
func TestAgentRunnerConfigKeepsTheMemoryFileWiring(t *testing.T) {
	projectRoot, log := memoryWiringFixture(t)

	cfg := agentRunnerConfig(agentRunDeps{
		cfg:    &config.Config{},
		memory: newMemoryWiring(log),
	})

	if cfg.MemoryFile == nil {
		t.Fatal("MemoryFile is nil: the runner would read a log nothing opens")
	}
	reader := cfg.MemoryFile()
	if want := filepath.Join(projectRoot, "Memory.md"); reader.Path() != want {
		t.Fatalf("MemoryFile path = %q, want %q", reader.Path(), want)
	}
	if err := reader.Append("an entry from the config's own reader", "cli"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	entries := reader.Tail(10)
	if len(entries) == 0 || !strings.Contains(entries[len(entries)-1], "an entry from the config's own reader") {
		t.Fatalf("Tail = %v, want the entry just appended", entries)
	}
}

// TestCliLearnChainIsNilOnlyWhenNothingIsWired is the checkpoint contract for
// memory-only learning.
// Nil only when NOTHING is wired: no extractor credential and no log. A
// credential with no log must still produce a chain, because the extractor is the
// half that writes memories rather than checkpoints, and a chain that vanishes
// when the log is absent is how a surface ends up reading a log it never appends
// to - the empty log reads as the agent having learned nothing.
func TestCliLearnChainIsNilOnlyWhenNothingIsWired(t *testing.T) {
	if chain := cliLearnChain(llm.ProviderConfig{}, nil); chain != nil {
		t.Error("chain is non-nil with no credential and no log, so a caller cannot tell wired from unwired")
	}
	log := memory.OpenMemoryFile(filepath.Join(t.TempDir(), "Memory.md"))
	if chain := cliLearnChain(llm.ProviderConfig{}, log); chain == nil {
		t.Error("chain is nil with a log configured, so checkpoint summaries would never be recorded")
	}
}
