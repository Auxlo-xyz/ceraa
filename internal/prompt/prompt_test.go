package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sourceprompt "ceraa/prompt"
)

func TestLoadUsesTheEmbeddedPrompt(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	t.Setenv("CERAA_SYSTEM_PROMPT", "")

	want := resolveSearchTool(string(sourceprompt.System), "embedded prompt")
	if got := load(); got != want {
		t.Errorf("load() differs from embedded prompt: %d vs %d bytes", len(got), len(want))
	}
}

func TestLoadIgnoresStaleDiskPrompt(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "prompt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt", "system.txt"), []byte("stale prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	t.Setenv("CERAA_SYSTEM_PROMPT", "")

	got := load()
	if got == "stale prompt" || !strings.Contains(got, "You are Ceraa") {
		t.Errorf("load() used the stale disk prompt: %q", got)
	}
}

func TestLoadPrefersTheOverride(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "mine.txt")
	if err := os.WriteFile(custom, []byte("my own prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CERAA_SYSTEM_PROMPT", custom)
	if got := load(); got != "my own prompt" {
		t.Errorf("load() = %q, want the override contents", got)
	}
}

func TestLoadIgnoresAnEmptyOverride(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CERAA_SYSTEM_PROMPT", empty)
	if got := load(); got == degraded || strings.TrimSpace(got) == "" {
		t.Error("an empty override did not fall back to the embedded prompt")
	}
}

func TestSystemIsNotTheFallback(t *testing.T) {
	if System == degraded {
		t.Fatal("System uses the minimal fallback despite the embedded prompt")
	}
	if !strings.Contains(System, "You are Ceraa") {
		t.Errorf("System does not look like the embedded prompt: %.80q", System)
	}
}
