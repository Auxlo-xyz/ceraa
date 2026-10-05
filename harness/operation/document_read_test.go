package operation

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The seam under test: a model read of an archive or document must come back
// extracted, not transcribed. Going through the manager rather than calling
// the document package directly is what proves the wiring, because a helper
// that is never reached from a read is a helper that does nothing.

func writeTestZip(t *testing.T, path string, members map[string]string) {
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

func runRead(t *testing.T, path string) ResultEvent {
	t.Helper()
	mgr := New(nil)
	mgr.Add(Operation{
		ID:     "test-doc",
		Spec:   Spec{Type: "read", Args: map[string]any{"path": path}},
		Status: StatusPending,
	})
	select {
	case r := <-mgr.Results():
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for read result")
		return ResultEvent{}
	}
}

func TestAReadUnpacksAZipInsteadOfTranscribingIt(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "bundle.zip")
	writeTestZip(t, archive, map[string]string{"notes/todo.txt": "buy milk"})
	result := runRead(t, archive)
	if result.Status != StatusCompleted {
		t.Fatalf("status = %s, error = %q", result.Status, result.Error)
	}
	if !strings.Contains(result.Output, "unpacked to") || !strings.Contains(result.Output, "todo.txt") {
		t.Errorf("a zip read did not come back as a listing:\n%s", result.Output)
	}
	if strings.Contains(result.Output, "PK\x03\x04") {
		t.Error("raw zip bytes leaked into the read output")
	}
	// The listed member path must itself be readable through the same seam.
	member := filepath.Join(dir, "bundle.extracted", "notes", "todo.txt")
	memberResult := runRead(t, member)
	if memberResult.Status != StatusCompleted || !strings.Contains(memberResult.Output, "buy milk") {
		t.Errorf("extracted member did not read back: status=%s output=%q error=%q",
			memberResult.Status, memberResult.Output, memberResult.Error)
	}
}

func TestAReadConvertsADocxToText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memo.docx")
	writeTestZip(t, path, map[string]string{
		"word/document.xml": `<?xml version="1.0"?><w:document xmlns:w="http://example"><w:body><w:p><w:r><w:t>quarterly targets</w:t></w:r></w:p></w:body></w:document>`,
	})
	result := runRead(t, path)
	if result.Status != StatusCompleted {
		t.Fatalf("status = %s, error = %q", result.Status, result.Error)
	}
	if !strings.Contains(result.Output, "quarterly targets") {
		t.Errorf("a docx read did not come back as text:\n%s", result.Output)
	}
}

func TestAReadOfAScannedPdfFailsWithTheActualGap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scan.pdf")
	raw := "%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n" +
		"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n" +
		"3 0 obj\n<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>\nendobj\n" +
		"4 0 obj\n<< /Length 4 >>\nstream\nq Q\nendstream\nendobj\ntrailer\n<< /Root 1 0 R >>\n"
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	result := runRead(t, path)
	if result.Status != StatusFailed {
		t.Fatalf("a scanned PDF read as success with output:\n%s", result.Output)
	}
	if !strings.Contains(result.Error, "OCR") {
		t.Errorf("the refusal does not name the gap: %q", result.Error)
	}
}
