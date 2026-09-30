package search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real session scoped two searches to /workspace/auxlo-neo and got back
// results from soraacode, the Python 3.14 standard library and a tcl9 tarball,
// because the scope was used to decide what to index and never what to return.
func TestSearchInRestrictsToTheGivenDirectory(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("auxlo-neo/src/index.ts", "export const name = 'AuxloNeo worker entry'\n")
	write("auxlo-neo/src/logger.ts", "AuxloNeo logging\n")
	write("cg-auxlo-xyz/src/api.ts", "AuxloNeo proxy client\n")
	write("soraacode/specs/todo.md", "AuxloNeo migration notes\n")

	idx, err := Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if _, err := idx.IndexDir(dir); err != nil {
		t.Fatal(err)
	}

	scoped, err := idx.SearchIn("AuxloNeo", filepath.Join(dir, "auxlo-neo"), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 2 {
		t.Fatalf("scoped search returned %d results, want the 2 under auxlo-neo: %+v", len(scoped), scoped)
	}
	scope := filepath.Join(dir, "auxlo-neo") + string(filepath.Separator)
	for _, r := range scoped {
		if !strings.HasPrefix(r.Path, scope) {
			t.Errorf("scoped search leaked %s, which is outside %s", r.Path, scope)
		}
	}
	// Unscoped still sees everything, because that is a legitimate request.
	all, err := idx.Search("AuxloNeo", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("unscoped search returned %d, want all 4", len(all))
	}
}

// A scoped search must still return `limit` matches. Filtering after the query
// would cap the scan first and then discard most of it, so a busy workspace
// would quietly return fewer results than asked for.
func TestAScopedSearchStillHonoursTheLimit(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "target")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// 5 in scope, 40 out of it.
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(sub, string(rune('a'+i))+".go"), []byte("needle here\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(dir, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		name := filepath.Join(other, string(rune('a'+i%26))+string(rune('a'+i/26))+".go")
		if err := os.WriteFile(name, []byte("needle here\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if _, err := idx.IndexDir(dir); err != nil {
		t.Fatal(err)
	}
	got, err := idx.SearchIn("needle", sub, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("scoped search with limit 20 returned %d, want all 5 in scope", len(got))
	}
}

// A string prefix would treat /workspace/auxlo as matching /workspace/auxlo-xyz,
// which is exactly the kind of neighbouring project that pollutes a workspace
// search.
func TestScopeIsADirectoryNotAStringPrefix(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{"auxlo/src/a.go", "auxlo-xyz/src/b.go"} {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if _, err := idx.IndexDir(dir); err != nil {
		t.Fatal(err)
	}
	got, err := idx.SearchIn("needle", filepath.Join(dir, "auxlo"), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want only the one inside auxlo: %+v", len(got), got)
	}
	if !strings.HasPrefix(got[0].Path, filepath.Join(dir, "auxlo")+string(filepath.Separator)) {
		t.Errorf("matched %s, which is in a sibling directory", got[0].Path)
	}
}

// A scope of / is the whole workspace and must not become a LIKE of "//%".
func TestRootScopeIsNotDoubleSlashed(t *testing.T) {
	if got := dirPrefix(string(filepath.Separator)); got != "/" {
		t.Errorf("dirPrefix(/) = %q, want a single separator", got)
	}
	if got := dirPrefix(""); got != "" {
		t.Errorf("dirPrefix(\"\") = %q, want empty for unscoped", got)
	}
	if got := dirPrefix("/tmp/x/"); got != "/tmp/x/" {
		t.Errorf("dirPrefix with a trailing separator = %q, want it left alone", got)
	}
}
