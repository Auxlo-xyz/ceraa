package search

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectIndexesAreIsolated(t *testing.T) {
	workspace := t.TempDir()
	projectA := filepath.Join(workspace, "project-a")
	projectB := filepath.Join(workspace, "project-b")
	if err := os.MkdirAll(projectA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectA, "main.go"), []byte("package main\nfunc migration() {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectB, "main.go"), []byte("package main\nfunc billing() {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := Open(ProjectIndexPath(workspace, projectA))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := first.IndexDir(projectA); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ProjectIndexPath(workspace, projectB))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.IndexDir(projectB); err != nil {
		t.Fatal(err)
	}

	results, err := first.Search("migration", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != filepath.Join(projectA, "main.go") {
		t.Fatalf("project A results = %+v", results)
	}
	results, err = first.Search("billing", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("project A leaked project B results: %+v", results)
	}
}

func TestIndexDirPrunesDeletedFiles(t *testing.T) {
	workspace := t.TempDir()
	project := filepath.Join(workspace, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "main.go")
	if err := os.WriteFile(path, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := Open(ProjectIndexPath(workspace, project))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if _, err := idx.IndexDir(project); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.IndexDir(project); err != nil {
		t.Fatal(err)
	}
	results, err := idx.Search("package", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("deleted file remained indexed: %+v", results)
	}
}
