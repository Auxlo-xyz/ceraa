package tool

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolvePathFromPrefersExplicitBase pins that a relative path resolves
// against the session workdir rather than the process directory.
//
// A shell tool runs in the session workdir, so a relative path that works in
// bash must work in a tool. Resolving against the process directory instead made
// a file the model had just created look missing.
func TestResolvePathFromPrefersExplicitBase(t *testing.T) {
	base := t.TempDir()
	got, err := ResolvePathFrom(base, "a/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(base, "a/b.txt") {
		t.Fatalf("ResolvePathFrom = %q, want it under %q", got, base)
	}
	absolute, err := ResolvePathFrom(base, "/etc/hosts")
	if err != nil {
		t.Fatal(err)
	}
	if absolute != "/etc/hosts" {
		t.Fatalf("an absolute path must be left alone, got %q", absolute)
	}
	fallback, err := ResolvePathFrom("", "rel.txt")
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	if fallback != filepath.Join(cwd, "rel.txt") {
		t.Fatalf("empty base = %q, want it under the process directory %q", fallback, cwd)
	}
}
