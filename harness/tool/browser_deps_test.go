package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The repair path downloads packages only after a launch already failed, so a
// mistake here is invisible on every working machine. These pin the two pieces
// that can be checked without a network: what gets unpacked, and what gets
// fetched for a given set of missing libraries.

func TestFlattenBrowserLibsKeepsTheSonameLinks(t *testing.T) {
	root := t.TempDir()
	libdir := filepath.Join(root, "usr", "lib", "x86_64-linux-gnu")
	if err := os.MkdirAll(filepath.Join(libdir, "nss"), 0o755); err != nil {
		t.Fatal(err)
	}
	versioned := filepath.Join(libdir, "libatk-1.0.so.0.25611.1")
	if err := os.WriteFile(versioned, []byte("atk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("libatk-1.0.so.0.25611.1", filepath.Join(libdir, "libatk-1.0.so.0")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libdir, "nss", "libnssckbi.so"), []byte("nss"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := t.TempDir()
	copied, err := flattenBrowserLibs(root, target)
	if err != nil {
		t.Fatalf("flattenBrowserLibs: %v", err)
	}
	if copied != 2 {
		t.Errorf("copied %d files, want the link and its target only", copied)
	}
	if got, err := os.ReadFile(filepath.Join(target, "libatk-1.0.so.0.25611.1")); err != nil || string(got) != "atk" {
		t.Errorf("versioned library = %q, %v; want the file copied", got, err)
	}
	if link, err := os.Readlink(filepath.Join(target, "libatk-1.0.so.0")); err != nil || link != "libatk-1.0.so.0.25611.1" {
		t.Errorf("soname link = %q, %v; want it recreated, not dereferenced", link, err)
	}
	if _, err := os.Stat(filepath.Join(target, "libnssckbi.so")); !os.IsNotExist(err) {
		t.Error("a directory below the multiarch directory was flattened; only its direct children belong in the search path")
	}
}

func TestBrowserDebNamesForCoversOnlyTheLibrariesThatAreMissing(t *testing.T) {
	names, unknown := browserDebNamesFor(map[string]bool{
		"libatk-1.0.so.0": true,
		"libmystery.so.9": true,
	})
	joined := strings.Join(names, " ")
	for _, want := range []string{"libatk1.0-0t64", "libatk1.0-0"} {
		if !strings.Contains(joined, want) {
			t.Errorf("candidates %q do not include %q", joined, want)
		}
	}
	if strings.Contains(joined, "libgbm1") {
		t.Errorf("candidates %q include a package whose library is not missing", joined)
	}
	if len(unknown) != 1 || unknown[0] != "libmystery.so.9" {
		t.Errorf("unknown = %v, want only libmystery.so.9", unknown)
	}
}

func TestEveryBrowserDebHasNamesAndNoSharedSonames(t *testing.T) {
	seen := make(map[string]string)
	for _, deb := range browserDebs {
		if len(deb.names) == 0 || len(deb.sonames) == 0 {
			t.Fatalf("package entry %v is incomplete", deb)
		}
		for _, soname := range deb.sonames {
			if other, ok := seen[soname]; ok {
				t.Errorf("%s is claimed by both %v and %v", soname, other, deb.names)
			}
			seen[soname] = strings.Join(deb.names, " or ")
		}
	}
}
