package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ceraa/harness/coordinator"
	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/internal/config"
	"ceraa/internal/paths"
)

// memoryWiringFixture builds the pieces a CLI run builds: a project root with a
// memory file, a store, and the reader for the root's append-only log.
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

// writeIdentityFile drops a BOND.md into the data directory, where the deleted
// renderer used to read it from, and removes it again on cleanup. Only a file
// that exists can prove the surface does not render it.
func writeIdentityFile(t *testing.T, body string) string {
	t.Helper()
	path := paths.BondFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Cleanup(func() { os.Remove(path) })
	return path
}

// assertNoBondRenderer walks a constructed RunnerConfig for any live renderer
// that is not the memory context. The BondFunc field the harness carried is
// gone, so this asserts the shape rather than a name: a renderer-shaped field
// that is set and is not MemoryContextFunc is a bond renderer under another
// name.
func assertNoBondRenderer(t *testing.T, cfg coordinator.RunnerConfig) {
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
		t.Errorf("RunnerConfig.%s is set: the bond renderer is gone and no second renderer may take its place", field.Name)
	}
}

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
	if strings.Contains(rendered, "<bond>") || strings.Contains(rendered, "the operator rides a green bicycle") {
		t.Errorf("memory context carries the bond, which no renderer writes any more:\n%s", rendered)
	}
}

// TestAgentRunnerConfigCarriesNoBondWiring pins the deletion on the config a
// CLI run actually receives, with an identity file on disk to make the claim
// observable: the only renderer left is the memory one, and it renders memory
// and nothing else.
func TestAgentRunnerConfigCarriesNoBondWiring(t *testing.T) {
	projectRoot, store, log := memoryWiringFixture(t)
	writeIdentityFile(t, "The operator rides a green bicycle to the office.\n")

	cfg := agentRunnerConfig(agentRunDeps{
		cfg:         &config.Config{},
		memory:      newMemoryWiring(store, projectRoot, "cli", log),
		memoryStore: store,
	})

	assertNoBondRenderer(t, cfg)
	assertRendersMemoryOnly(t, cfg)
}

// TestAgentRunnerConfigKeepsTheMemoryFileWiring pins what this deletion must
// not touch: the reader for the append-only log, on the same config, at the
// project root the wiring was built against.
func TestAgentRunnerConfigKeepsTheMemoryFileWiring(t *testing.T) {
	projectRoot, store, log := memoryWiringFixture(t)

	cfg := agentRunnerConfig(agentRunDeps{
		cfg:         &config.Config{},
		memory:      newMemoryWiring(store, projectRoot, "cli", log),
		memoryStore: store,
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

// TestCliLearnChainCarriesNoBondLearner is the checkpoint contract after the
// bond learner is gone. With no extraction credential the chain is nil, so no
// hook hides behind a disabled extractor; with one configured it runs the
// extractor and nothing else, and the identity file is neither appended to nor
// rewritten.
// Nil only when NOTHING is wired: no extractor credential and no log. A
// credential with no log must still produce a chain, because the extractor is the
// half that writes memories rather than checkpoints, and a chain that vanishes
// when the log is absent is how a surface ends up reading a log it never appends
// to - the empty log reads as the agent having learned nothing.
func TestCliLearnChainIsNilOnlyWhenNothingIsWired(t *testing.T) {
	store, err := memory.New(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("memory store: %v", err)
	}
	if chain := cliLearnChain(llm.ProviderConfig{}, store, nil); chain != nil {
		t.Error("chain is non-nil with no credential and no log, so a caller cannot tell wired from unwired")
	}
	log := memory.OpenMemoryFile(filepath.Join(t.TempDir(), "Memory.md"))
	if chain := cliLearnChain(llm.ProviderConfig{}, store, log); chain == nil {
		t.Error("chain is nil with a log configured, so checkpoint summaries would never be recorded")
	}
}
