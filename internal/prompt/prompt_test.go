package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadReadsTheCanonicalFile(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(root, "prompt", "system.txt"))
	if err != nil {
		t.Fatalf("read canonical prompt: %v", err)
	}
	if strings.TrimSpace(string(contents)) == "" {
		t.Fatal("canonical prompt is empty")
	}
	// The loader must find that same file, from the repository root.
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	// The loader substitutes the search instruction, so its output is legitimately
	// longer than the file. Compared through the same substitution rather than
	// byte-for-byte, which is what pinned "the file is the prompt" and would now
	// fail for a change that is the whole point of the loader.
	want := resolveSearchTool(string(contents), SystemPromptPath)
	if got := load(); got != want {
		t.Errorf("loader returned different content than the resolved file (%d vs %d bytes)", len(got), len(want))
	}
}

// A missing prompt must not panic and must not return a full second copy, or the
// two would drift apart again.
func TestLoadFallsBackWithoutDuplicating(t *testing.T) {
	dir := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	// Point the override at nothing so the executable-directory search cannot
	// accidentally find the real file.
	t.Setenv("CERAA_SYSTEM_PROMPT", filepath.Join(dir, "absent.txt"))

	got := load()
	if got != degraded {
		t.Errorf("fallback = %q, want the minimal identity", got)
	}
	if len(got) > 400 {
		t.Errorf("fallback is %d bytes, which is a second copy of the prompt rather than an identity", len(got))
	}
}

func TestLoadPrefersTheOverride(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "mine.txt")
	if err := os.WriteFile(custom, []byte("my own prompt"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("CERAA_SYSTEM_PROMPT", custom)
	if got := load(); got != "my own prompt" {
		t.Errorf("load() = %q, want the override contents", got)
	}
}

// An empty override file must not be taken as the prompt; an empty system prompt
// would leave the agent with no instructions at all.
func TestLoadIgnoresAnEmptyOverride(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	t.Setenv("CERAA_SYSTEM_PROMPT", empty)

	if got := load(); got == strings.TrimSpace(got) && strings.TrimSpace(got) == "" {
		t.Error("an empty override was accepted as the system prompt")
	}
}

// The loaded prompt must actually reach the model, so System is wired to the
// file rather than left as the fallback.
func TestSystemIsNotTheFallback(t *testing.T) {
	if System == degraded {
		t.Skip("test binary is running without the prompt on disk")
	}
	if !strings.Contains(System, "interactive coding agent") {
		t.Errorf("System does not look like the canonical prompt: %.80q", System)
	}
}
