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
	"ceraa/internal/paths"
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

// writeIdentityFile drops a BOND.md into the data directory, where the deleted
// renderer used to read it from, and removes it again on cleanup.
//
// The file is the whole point of the fixture: a surface that still wired the
// bond would render it into the memory block, and only a file that exists can
// prove it does not.
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
// that is not the memory context.
//
// The BondFunc field the harness carried is gone, so this asserts the shape
// rather than a name: a renderer-shaped field that is set and is not
// MemoryContextFunc is a bond renderer under another name, and it has to fail
// here rather than reach the prompt as a second channel.
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

// assertRendersMemoryOnly is the contract the bond deletion has to keep: the
// memory block still renders the project's memory, and the identity file that
// sits in the data directory is not part of it.
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
// Telegram run actually receives, with an identity file on disk to make the
// claim observable: the only renderer left is the memory one, and it renders
// memory and nothing else.
func TestAgentRunnerConfigCarriesNoBondWiring(t *testing.T) {
	projectRoot, store, log := memoryWiringFixture(t)
	writeIdentityFile(t, "The operator rides a green bicycle to the office.\n")

	cfg := agentRunnerConfig(agentRunDeps{
		sessionID:     "tg-bond-1",
		cfg:           &config.Config{},
		memoryContext: "startup snapshot",
		memory:        newMemoryWiring(store, projectRoot, "tg-bond-1", log),
		memoryStore:   store,
	})

	assertNoBondRenderer(t, cfg)
	assertRendersMemoryOnly(t, cfg)
}

// TestAgentRunnerConfigKeepsTheMemoryFileWiring pins what this deletion must
// not touch: the reader for the append-only log, on the same config, at the
// project root.
func TestAgentRunnerConfigKeepsTheMemoryFileWiring(t *testing.T) {
	projectRoot, store, log := memoryWiringFixture(t)

	cfg := agentRunnerConfig(agentRunDeps{
		sessionID:   "tg-bond-2",
		cfg:         &config.Config{},
		memory:      newMemoryWiring(store, projectRoot, "tg-bond-2", log),
		memoryStore: store,
	})

	if cfg.MemoryFile == nil {
		t.Fatal("MemoryFile is nil: the runner would read a log nothing opens")
	}
	reader := cfg.MemoryFile()
	if want := filepath.Join(projectRoot, "Memory.md"); reader.Path() != want {
		t.Fatalf("MemoryFile path = %q, want %q", reader.Path(), want)
	}
	if err := reader.Append("an entry from the config's own reader", "tg-bond-2"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	entries := reader.Tail(10)
	if len(entries) == 0 || !strings.Contains(entries[len(entries)-1], "an entry from the config's own reader") {
		t.Fatalf("Tail = %v, want the entry just appended", entries)
	}
}

// TestAgentLearnChainWritesTheLogAndNeverTheIdentityFile is the checkpoint
// contract after the bond learner is gone: the chain still writes the log, and
// the identity file the bond learner maintained is left alone - not appended
// to, not rewritten.
func TestAgentLearnChainWritesTheLogAndNeverTheIdentityFile(t *testing.T) {
	projectRoot, store, log := memoryWiringFixture(t)
	bondPath := writeIdentityFile(t, "The operator rides a green bicycle to the office.\n")
	before, err := os.ReadFile(bondPath)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}

	chain := agentLearnChain(llm.ProviderConfig{BaseURL: "http://127.0.0.1:1", Model: "unused"}, store, projectRoot, log)
	if chain == nil {
		t.Fatal("learn chain is nil, so the append-only log would never be written")
	}
	chain(context.Background(), "tg-bond-3", nil, "a checkpoint worth recording")

	entries := log.Tail(10)
	if len(entries) == 0 || !strings.Contains(entries[len(entries)-1], "a checkpoint worth recording") {
		t.Fatalf("Tail = %v, want the summary the compaction produced", entries)
	}
	after, err := os.ReadFile(bondPath)
	if err != nil {
		t.Fatalf("read identity file: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("identity file changed from %q to %q: the learn chain still writes a bond", before, after)
	}
}

// TestAgentLearnChainRunsTheExtractorAndNoBond is the chain with extraction
// configured: the extractor still runs off the checkpoint and the log is still
// written, with no third hook beside them.
//
// The extractor is called with nothing discarded, so it makes no model call:
// the point is the chain's membership, not the extractor's own behaviour.
func TestAgentLearnChainRunsTheExtractorAndNoBond(t *testing.T) {
	t.Setenv("CERAA_EXTRACT_KEY", "test-extract-key")
	projectRoot, store, log := memoryWiringFixture(t)
	identity := writeIdentityFile(t, "The operator rides a green bicycle to the office.\n")

	chain := agentLearnChain(llm.ProviderConfig{BaseURL: "http://127.0.0.1:1", Model: "unused"}, store, projectRoot, log)
	if chain == nil {
		t.Fatal("learn chain is nil with an extraction credential configured, so the extractor is no longer wired")
	}
	chain(context.Background(), "tg-bond-4", nil, "a second checkpoint")

	entries := log.Tail(10)
	if len(entries) == 0 || !strings.Contains(entries[len(entries)-1], "a second checkpoint") {
		t.Fatalf("Tail = %v, want the summary the compaction produced", entries)
	}
	if raw, err := os.ReadFile(identity); err != nil || !strings.Contains(string(raw), "green bicycle") {
		t.Fatalf("identity file = %q (err %v), want the fixture untouched", raw, err)
	}
	if _, err := os.Stat(paths.BondFile()); err != nil {
		t.Fatalf("stat identity file: %v", err)
	}
}

// TestWebLiveRunnerConfigCarriesNoBondWiring is the dashboard's half of the
// same claim: the config the compaction button previews from carries the memory
// renderer and the log reader, and no bond renderer of either kind.
func TestWebLiveRunnerConfigCarriesNoBondWiring(t *testing.T) {
	projectRoot, store, log := memoryWiringFixture(t)
	writeIdentityFile(t, "The operator rides a green bicycle to the office.\n")

	cfg := webLiveRunnerConfig(webLiveDeps{
		sessionID:   "web-bond-1",
		cfg:         &config.Config{},
		projectRoot: projectRoot,
		memory:      newMemoryWiring(store, projectRoot, "web-bond-1", log),
		memoryStore: store,
	})

	assertNoBondRenderer(t, cfg)
	assertRendersMemoryOnly(t, cfg)
	if cfg.MemoryFile == nil {
		t.Fatal("MemoryFile is nil: the previewed request would read a log nothing opens")
	}
	if want := filepath.Join(projectRoot, "Memory.md"); cfg.MemoryFile().Path() != want {
		t.Fatalf("MemoryFile path = %q, want %q", cfg.MemoryFile().Path(), want)
	}
}
