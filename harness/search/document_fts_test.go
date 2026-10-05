package search

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func writeSearchZip(t *testing.T, path string, members map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for name, content := range members {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Documents must be searchable by what they say, and archives by what they
// hold. A docx that indexes as binary is invisible to search, which is the
// behaviour this replaces.
func TestDocumentsIndexByExtractedText(t *testing.T) {
	workspace := t.TempDir()
	project := filepath.Join(workspace, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSearchZip(t, filepath.Join(project, "memo.docx"), map[string]string{
		"word/document.xml": `<?xml version="1.0"?><w:document xmlns:w="http://example"><w:body><w:p><w:r><w:t>quarterly capybara census</w:t></w:r></w:p></w:body></w:document>`,
	})
	writeSearchZip(t, filepath.Join(project, "bundle.zip"), map[string]string{
		"notes/capybara-notes.txt": "the capybara keeper logs",
	})
	idx, err := Open(ProjectIndexPath(workspace, project))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if _, err := idx.IndexDir(project); err != nil {
		t.Fatal(err)
	}
	results, err := idx.Search("capybara", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, r := range results {
		found[filepath.Base(r.Path)] = true
	}
	// The docx matches on its extracted text. The zip matches on the member
	// filename, and the extracted member matches on its own content: three
	// different paths to the same answer, all of which must work.
	if !found["memo.docx"] {
		t.Errorf("the docx is not searchable by its text; results = %+v", results)
	}
	if !found["bundle.zip"] {
		t.Errorf("the archive is not searchable by member name; results = %+v", results)
	}
	if !found["capybara-notes.txt"] {
		t.Errorf("the extracted member is not searchable; results = %+v", results)
	}
}
