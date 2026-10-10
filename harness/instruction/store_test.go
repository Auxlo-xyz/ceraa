package instruction

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/internal/paths"
)

// writeFile builds a fixture project file.
func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make fixture dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

// touch moves a file's modification time forward without changing a byte.
func touch(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("touch fixture: %v", err)
	}
}

func TestTheStoreDoesNotReparseAFileThatDidNotChange(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "AGENTS.md", "# Style\n\n- Go only, no TypeScript\n")
	store := NewStore(dir, "AGENTS.md")

	if snap := store.Refresh(); snap.Len() != 1 {
		t.Fatalf("first refresh compiled %d rules", snap.Len())
	}
	if got := store.Stats().Compiles; got != 1 {
		t.Fatalf("first refresh compiled %d times, want 1", got)
	}
	// The cache goes first so this measures the stat gate rather than the cache.
	// With a cache in place a reparse would be free, and a test that cannot tell
	// a cheap gate from an expensive one pins neither.
	if err := os.Remove(CachePath(dir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		store.Refresh()
	}
	if got := store.Stats().Compiles; got != 1 {
		t.Fatalf("three further refreshes of an unchanged file compiled %d times in total, want 1", got)
	}
	if got := store.Stats().CacheReads; got != 0 {
		t.Fatalf("an unchanged file was re-read: %d cache reads", got)
	}
	if got := store.Stats().Reads; got != 1 {
		t.Fatalf("the stat gate opened the file %d times, want the single read of the first refresh", got)
	}
}

func TestATouchedButIdenticalFileIsNotRecompiled(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "AGENTS.md", "- Go only, no TypeScript\n")
	store := NewStore(dir, "AGENTS.md")
	before := store.Refresh()
	if store.Stats().Compiles != 1 {
		t.Fatalf("first refresh compiled %d times", store.Stats().Compiles)
	}

	// The on-disk cache is removed first, so this measures the hash gate and not
	// the cache. With a cache present a recompile would be free anyway, and a
	// test that cannot tell those two apart pins neither of them.
	if err := os.Remove(CachePath(dir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}

	// A checkout, a restore from a snapshot, or a filesystem that rounds mtimes
	// all look like this: a new timestamp over identical bytes.
	touch(t, filepath.Join(dir, "AGENTS.md"), time.Now().Add(2*time.Hour))

	after := store.Refresh()
	if store.Stats().Compiles != 1 {
		t.Fatalf("a touched but identical file was recompiled: %d compiles", store.Stats().Compiles)
	}
	if store.Stats().CacheReads != 0 {
		t.Fatalf("the hash gate did not stop the refresh before the cache: %+v", store.Stats())
	}
	if store.Stats().Reads != 2 {
		t.Fatalf("a touched file should be read exactly once more, got %d reads in total", store.Stats().Reads)
	}
	if after.Digest != before.Digest || after.Len() != before.Len() {
		t.Fatal("the published snapshot changed when no byte of the file changed")
	}
}

func TestAnEditedFileIsRecompiledAndTheNewRuleArrives(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "AGENTS.md", "- Go only, no TypeScript\n")
	store := NewStore(dir, "AGENTS.md")
	store.Refresh()

	writeFile(t, dir, "AGENTS.md", "- Go only, no TypeScript\n- Never write under the source tree\n")
	snap := store.Refresh()
	if snap.Len() != 2 {
		t.Fatalf("after an edit the store holds %d rules, want 2", snap.Len())
	}
	if store.Stats().Compiles != 2 {
		t.Fatalf("an edit compiled %d times in total, want 2", store.Stats().Compiles)
	}
}

func TestASecondStoreReusesTheCacheInsteadOfRecompiling(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "AGENTS.md", "# Style\n\n- Go only, no TypeScript\n")
	first := NewStore(dir, "AGENTS.md")
	original := first.Refresh()
	if first.Stats().CacheWrites != 1 {
		t.Fatalf("the first refresh wrote %d caches, want 1", first.Stats().CacheWrites)
	}

	second := NewStore(dir, "AGENTS.md")
	cached := second.Refresh()
	if second.Stats().Compiles != 0 {
		t.Fatalf("a restart recompiled %d rules instead of reading the cache", second.Stats().Compiles)
	}
	if second.Stats().CacheReads != 1 {
		t.Fatalf("cache reads = %d, want 1", second.Stats().CacheReads)
	}
	if cached.Digest != original.Digest || cached.Len() != original.Len() {
		t.Fatal("the cached snapshot is not the one that was compiled")
	}
	if cached.Instructions[0].ID != original.Instructions[0].ID {
		t.Fatal("a cached rule has a different id from the one that was compiled")
	}
}

func TestACorruptCacheIsDiscardedRatherThanFatal(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "AGENTS.md", "- Go only, no TypeScript\n")
	first := NewStore(dir, "AGENTS.md")
	first.Refresh()

	cache := CachePath(dir, "AGENTS.md")
	if err := os.WriteFile(cache, []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	second := NewStore(dir, "AGENTS.md")
	if second.Stats().CacheDiscards != 1 {
		t.Fatalf("a corrupt cache was not discarded at construction: %+v", second.Stats())
	}
	snap := second.Refresh()
	if snap.Len() != 1 {
		t.Fatalf("a corrupt cache cost the rules: %d compiled", snap.Len())
	}
	if second.Stats().Compiles != 1 {
		t.Fatalf("a corrupt cache did not fall back to a cold compile: %+v", second.Stats())
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("the corrupt cache was not removed: %v", err)
	}
}

func TestACacheFromADifferentFileIsNotTrusted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "AGENTS.md", "- Go only, no TypeScript\n")
	first := NewStore(dir, "AGENTS.md")
	first.Refresh()

	// Same size, different bytes: the case a stat gate alone would miss.
	writeFile(t, dir, "AGENTS.md", "- Go only, no TypeScripy\n")
	second := NewStore(dir, "AGENTS.md")
	snap := second.Refresh()
	if snap.Len() != 1 || !strings.Contains(snap.Instructions[0].Text, "TypeScripy") {
		t.Fatalf("a stale cache was served: %+v", snap.Instructions)
	}
}

func TestTheCacheIsWrittenUnderTheDataDirectoryAndNotToTmp(t *testing.T) {
	// The suite runs with CERAA_HOME pointed at a temporary directory, which is
	// itself under /tmp. This test therefore clears it, pins HOME to a directory
	// of its own and points every temporary-variable name at an empty scratch
	// directory, so "did the store use a temp file" is a question with an
	// observable answer rather than an assumption.
	home := t.TempDir()
	scratch := t.TempDir()
	t.Setenv("CERAA_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", scratch)
	t.Setenv("TMP", scratch)
	t.Setenv("TEMP", scratch)

	dir := t.TempDir()
	writeFile(t, dir, "AGENTS.md", "- Go only, no TypeScript\n")

	if got, want := filepath.Dir(CachePath(dir, "AGENTS.md")), filepath.Join(paths.DataDir(), "instructions"); got != want {
		t.Fatalf("the cache directory is %q, want %q", got, want)
	}

	store := NewStore(dir, "AGENTS.md")
	store.Refresh()
	if store.Stats().CacheWrites != 1 {
		t.Fatalf("nothing was cached: %+v", store.Stats())
	}
	if _, err := os.Stat(CachePath(dir, "AGENTS.md")); err != nil {
		t.Fatalf("no cache under the data directory: %v", err)
	}

	left, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range left {
		t.Errorf("the store wrote %s into the temporary directory", entry.Name())
	}
	// And the same check against every variable a caller might have set instead,
	// so a future edit that reaches for os.TempDir directly is caught here too.
	if !strings.HasPrefix(CachePath(dir, "AGENTS.md"), home) {
		t.Fatalf("the cache is not under the home the data directory resolves to")
	}
}

func TestAMissingSourceKeepsTheRulesTheProjectAlreadyHad(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "AGENTS.md", "- Go only, no TypeScript\n")
	writeFile(t, dir, "memory/AGENTS.md", "- Workspace rules\n")
	store := NewStore(dir, "AGENTS.md", "memory/AGENTS.md")
	if got := store.Refresh(); got.Len() != 2 {
		t.Fatalf("want two rules, got %d", got.Len())
	}

	if err := os.Remove(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	got := store.Refresh()
	if got.Len() != 1 {
		t.Fatalf("a withdrawn source left %d rules behind, want the one remaining source", got.Len())
	}
	if got.Instructions[0].Source != "memory/AGENTS.md" {
		t.Fatalf("the wrong source survived: %+v", got.Instructions)
	}
	if diags := store.Diagnostics(); len(diags) != 0 {
		t.Fatalf("a deleted source reported %v", diags)
	}
}

func TestAStoreWithNoSourcesIsEmptyAndNotBroken(t *testing.T) {
	store := NewStore(t.TempDir())
	snap := store.Refresh()
	if snap.Len() != 0 {
		t.Fatalf("a store with no sources compiled %d rules", snap.Len())
	}
	if got := Select(snap, Trigger{Targets: []string{"/srv/x"}}, NewSeen(), Options{Matcher: stubMatcher}); len(got.Rules) != 0 {
		t.Fatalf("an empty store selected %d rules", len(got.Rules))
	}
}

func TestDiagnosticsSurviveARepublish(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "AGENTS.md", "```bash\n- swallowed\n")
	store := NewStore(dir, "AGENTS.md")
	store.Refresh()
	diags := store.Diagnostics()
	if !diags.Has(DiagUnclosedFence) {
		t.Fatalf("the swallowed tail was not reported by the store: %v", diags)
	}
	if diags[0].Source != "AGENTS.md" {
		t.Errorf("the diagnostic names source %q", diags[0].Source)
	}
}

func TestTwoSourcesThatAgreeProduceTheSameMergedDigest(t *testing.T) {
	body := "- Go only, no TypeScript\n"
	first := t.TempDir()
	second := t.TempDir()
	writeFile(t, first, "AGENTS.md", body)
	writeFile(t, second, "AGENTS.md", body)
	a := NewStore(first, "AGENTS.md").Refresh()
	b := NewStore(second, "AGENTS.md").Refresh()
	if a.Digest != b.Digest {
		t.Fatalf("identical files produced different digests: %q and %q", a.Digest, b.Digest)
	}

	writeFile(t, second, "AGENTS.md", body+"- and one more\n")
	if c := NewStore(second, "AGENTS.md").Refresh(); c.Digest == a.Digest {
		t.Fatal("an edited file produced the same merged digest")
	}
}
