package tui

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

// memoryWiringFixture builds the pieces a turn builds: a project root with a
// memory file, a store, and the reader for the root's append-only log.
//
// The memory file is written for real, because "MemoryContextFunc still works"
// is a claim about rendered text: a config whose renderer answers with nothing
// would pass a nil check and fail the model.
func memoryWiringFixture(t *testing.T, projectRoot string) (store *memory.Store, log *memory.MemoryFile) {
	t.Helper()
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
	return store, log
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

// newMemoryWiringSession builds a session whose workspace root is a temporary
// directory. The default root is the operator's real one, and a fixture writing
// a memory file into it would edit the machine rather than the test.
func newMemoryWiringSession(t *testing.T) *Session {
	t.Helper()
	dir := t.TempDir()
	srv := stubProvider(t, func(int32) []string { return nil })
	cfg := &config.Config{
		WorkspaceRoot:  dir,
		SessionFile:    filepath.Join(dir, "sessions"),
		MemoryFile:     filepath.Join(dir, "memory"),
		SkillFile:      filepath.Join(dir, "skills"),
		EvaluationFile: filepath.Join(dir, "evals.json"),
		ToolOutputDir:  filepath.Join(dir, "toolout"),
		LLM: config.LLMProvider{
			URL:    srv.URL,
			Model:  "stub/model",
			APIKey: "test",
		},
	}
	var out strings.Builder
	s, err := NewSession(SessionOptions{
		Config:    cfg,
		Renderer:  NewRenderer(&out, Plain()),
		SessionID: "tui-memory-wiring",
		// Deny every permission, so a test can never block on a prompt.
		ReadLine: func(string) (string, error) { return "n", nil },
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return s
}

// TestSessionRunnerConfigCarriesMemoryWiring pins the config a turn actually
// receives: the only renderer left is the memory one, and it renders memory
// and nothing else.
func TestSessionRunnerConfigCarriesMemoryWiring(t *testing.T) {
	s := newMemoryWiringSession(t)
	store, log := memoryWiringFixture(t, s.workDir)

	provider := s.cfg.ActiveLLM()
	cfg := s.runnerConfig(sessionRunnerDeps{
		provider:      provider,
		modelConfig:   llm.FromProvider(provider.URL, provider.APIKey, provider.Model),
		memoryStore:   store,
		memoryContext: "startup snapshot",
		memory:        newMemoryWiring(store, s.workDir, s.session, log),
		skillContext:  "skill catalogue",
	})

	assertNoExtraRenderer(t, cfg)
	assertRendersMemoryOnly(t, cfg)
}

// TestSessionRunnerConfigKeepsTheMemoryFileWiring pins what this deletion must
// not touch: the reader for the append-only log, on the same config, at the
// session's workdir.
func TestSessionRunnerConfigKeepsTheMemoryFileWiring(t *testing.T) {
	s := newMemoryWiringSession(t)
	store, log := memoryWiringFixture(t, s.workDir)

	cfg := s.runnerConfig(sessionRunnerDeps{
		memory: newMemoryWiring(store, s.workDir, s.session, log),
	})

	if cfg.MemoryFile == nil {
		t.Fatal("MemoryFile is nil: the runner would read a log nothing opens")
	}
	reader := cfg.MemoryFile()
	if want := filepath.Join(s.workDir, "Memory.md"); reader.Path() != want {
		t.Fatalf("MemoryFile path = %q, want %q", reader.Path(), want)
	}
	if err := reader.Append("an entry from the config's own reader", s.session); err != nil {
		t.Fatalf("Append: %v", err)
	}
	entries := reader.Tail(10)
	if len(entries) == 0 || !strings.Contains(entries[len(entries)-1], "an entry from the config's own reader") {
		t.Fatalf("Tail = %v, want the entry just appended", entries)
	}
}

// TestCliLearnChainIsMemoryOnly is the checkpoint contract for memory-only
// learning. With no extraction credential the chain is nil, so no hook hides
// behind a disabled extractor; with one configured it runs the extractor and
// nothing else.
func TestCliLearnChainIsMemoryOnly(t *testing.T) {
	store, err := memory.New(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("memory store: %v", err)
	}
	if chain := cliLearnChain(llm.ProviderConfig{}, store, nil); chain != nil {
		t.Errorf("chain is non-nil with no credential and no log, so a caller cannot tell wired from unwired")
	}
	if chain := cliLearnChain(llm.ProviderConfig{}, store, memory.OpenMemoryFile(filepath.Join(t.TempDir(), "Memory.md"))); chain == nil {
		t.Error("chain is nil with a log configured, so checkpoint summaries would never be recorded")
	}

	t.Setenv("CERAA_EXTRACT_KEY", "test-extract-key")
	chain := cliLearnChain(llm.ProviderConfig{BaseURL: "http://127.0.0.1:1", Model: "unused"}, store, memory.OpenMemoryFile(filepath.Join(t.TempDir(), "Memory.md")))
	if chain == nil {
		t.Fatal("chain is nil with an extraction credential configured, so the extractor is no longer wired")
	}
	// Nothing discarded, so the extractor makes no model call: the claim under
	// test is the chain's membership, not the extractor's behaviour.
	chain(context.Background(), "tui-session", nil, "a checkpoint worth recording")
}
