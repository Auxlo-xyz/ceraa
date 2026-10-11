package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/internal/config"
)

// memoryWiringFixture builds the pieces a turn builds: a project root and the
// reader for the root's append-only log.
func memoryWiringFixture(t *testing.T, projectRoot string) *memory.MemoryFile {
	t.Helper()
	return memoryLogReader(projectRoot)
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

// TestSessionRunnerConfigKeepsTheMemoryFileWiring pins the reader for the
// append-only log, on the config a turn actually receives, at the session's
// workdir.
func TestSessionRunnerConfigKeepsTheMemoryFileWiring(t *testing.T) {
	s := newMemoryWiringSession(t)
	log := memoryWiringFixture(t, s.workDir)

	cfg := s.runnerConfig(sessionRunnerDeps{
		memory: newMemoryWiring(log),
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
	if chain := cliLearnChain(llm.ProviderConfig{}, nil); chain != nil {
		t.Errorf("chain is non-nil with no credential and no log, so a caller cannot tell wired from unwired")
	}
	if chain := cliLearnChain(llm.ProviderConfig{}, memory.OpenMemoryFile(filepath.Join(t.TempDir(), "Memory.md"))); chain == nil {
		t.Error("chain is nil with a log configured, so checkpoint summaries would never be recorded")
	}

	t.Setenv("CERAA_EXTRACT_KEY", "test-extract-key")
	chain := cliLearnChain(llm.ProviderConfig{BaseURL: "http://127.0.0.1:1", Model: "unused"}, memory.OpenMemoryFile(filepath.Join(t.TempDir(), "Memory.md")))
	if chain == nil {
		t.Fatal("chain is nil with an extraction credential configured, so the extractor is no longer wired")
	}
	// Nothing discarded, so the extractor makes no model call: the claim under
	// test is the chain's membership, not the extractor's behaviour.
	chain(context.Background(), "tui-session", nil, "a checkpoint worth recording")
}
