package document

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures are built by hand in code, never checked in as binaries. A binary
// fixture cannot be read in review, so a test that depends on one asserts
// nothing a reader can verify.

// writeZip builds a zip file from a name-to-content map.
func writeZip(t *testing.T, path string, members map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	// Sorted so the fixture bytes are stable across map orderings.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, name := range names {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(members[name])); err != nil {
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

func TestKindForPathNamesEverySupportedFormat(t *testing.T) {
	cases := map[string]Kind{
		"a.zip": KindArchive, "A.ZIP": KindArchive,
		"a.pdf": KindPDF, "a.docx": KindWord, "a.pptx": KindSlides,
	}
	for name, want := range cases {
		if got, ok := KindForPath(name); !ok || got != want {
			t.Errorf("KindForPath(%q) = %q, %v; want %q, true", name, got, ok, want)
		}
	}
	for _, name := range []string{"a.txt", "a.png", "a.tar.gz", "a.xlsx", "noext"} {
		if _, ok := KindForPath(name); ok {
			t.Errorf("KindForPath(%q) matched, want no match", name)
		}
	}
}

func TestAnArchiveListsAndUnpacksItsMembers(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "bundle.zip")
	writeZip(t, archive, map[string]string{
		"notes/todo.txt": "buy milk",
		"main.go":        "package main",
	})
	members, err := ExtractArchive(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("got %d members, want 2", len(members))
	}
	for _, m := range members {
		if m.Skipped != "" {
			t.Errorf("member %q skipped: %s", m.Path, m.Skipped)
		}
		raw, err := os.ReadFile(m.Path)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) == 0 {
			t.Errorf("member %q extracted empty", m.Path)
		}
	}
	// A second call reuses the extraction instead of redoing it.
	again, err := ExtractArchive(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 || again[0].Path != members[0].Path {
		t.Error("the second extraction did not reuse the first")
	}
	summary, err := Summarize(archive)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "todo.txt") || !strings.Contains(summary, "Read a member path") {
		t.Errorf("summary does not list members and the next step:\n%s", summary)
	}
}

// A hostile path must be refused and reported, not cleaned into a safe name.
// Cleaning would keep the entry under a name nobody recognises, which is how
// an exfiltrated file hides in a listing.
func TestAHostilePathIsRefusedNotCleaned(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.zip")
	writeZip(t, archive, map[string]string{
		"../escape.txt":    "out",
		"/absolute.txt":    "out",
		"..\\win.txt":      "out",
		"fine/ok.txt":      "in",
		"nested/inner.zip": "PK",
	})
	members, err := ExtractArchive(archive)
	if err != nil {
		t.Fatal(err)
	}
	refused := 0
	for _, m := range members {
		if m.Skipped != "" {
			refused++
			continue
		}
		rel, err := filepath.Rel(filepath.Join(dir, "evil.extracted"), m.Path)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("member escaped the destination: %q", m.Path)
		}
	}
	if refused != 4 {
		t.Errorf("refused %d members, want 4 (3 hostile paths plus the nested archive)", refused)
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); !os.IsNotExist(err) {
		t.Error("a hostile member escaped the destination")
	}
}

// The header size is a claim. A member that inflates past the budget must stop
// the extraction rather than fill the disk, which is exactly how a bomb works.
func TestAnArchiveBombIsStoppedByBytesWritten(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "bomb.zip")
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	// Six members of 12 MB each: every one passes the 16 MB per-file limit,
	// but together they inflate 72 MB against a 64 MB budget. Highly
	// compressible so the fixture stays kilobytes on disk.
	chunk := bytes.Repeat([]byte("a"), 12*1024*1024)
	for i := 0; i < 6; i++ {
		entry, err := writer.Create(fmt.Sprintf("part%d.bin", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractArchive(archive); err == nil {
		t.Fatal("a 72 MB inflation passed a 64 MB budget without an error")
	} else if !strings.Contains(err.Error(), "64 MB") {
		t.Fatalf("wrong refusal: %v", err)
	}
}

func TestTooManyFilesAreRefusedUpFront(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "many.zip")
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for i := 0; i < maxArchiveFiles+1; i++ {
		entry, err := writer.Create(fmt.Sprintf("f%04d.txt", i))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = entry.Write([]byte("x"))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractArchive(archive); err == nil {
		t.Fatal("1001 files passed a 1000 file limit without an error")
	}
}

const wordDocumentXML = `<?xml version="1.0"?>
<w:document xmlns:w="http://schemas.openxmlformatschemas.org/wordprocessingml/2006/main">
<w:body>
<w:p><w:r><w:t>First paragraph</w:t></w:r><w:r><w:t> continued</w:t></w:r></w:p>
<w:p><w:r><w:t>Second</w:t></w:r></w:p>
<w:tbl><w:tr><w:tc><w:p><w:r><w:t>cell one</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>cell two</w:t></w:r></w:p></w:tc></w:tr></w:tbl>
</w:body>
</w:document>`

func TestADocxReadsParagraphsAndTablesInOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memo.docx")
	writeZip(t, path, map[string]string{
		"word/document.xml": wordDocumentXML,
		"word/header1.xml":  `<?xml version="1.0"?><w:hdr xmlns:w="http://example"><w:p><w:r><w:t>Acme Corp</w:t></w:r></w:p></w:hdr>`,
	})
	text, err := ExtractText(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"First paragraph continued", "Second", "cell one", "cell two", "Acme Corp"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Index(text, "cell one") > strings.Index(text, "cell two") {
		t.Error("table cells read out of order")
	}
}

const slideXMLTemplate = `<?xml version="1.0"?>
<p:sld xmlns:a="http://schemas.openxmlformatschemas.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformatschemas.org/presentationml/2006/main">
<p:cSld><p:spTree>
<p:sp><p:txBody><a:p><a:r><a:t>%s</a:t></a:r></a:p></p:txBody></p:sp>
</p:spTree></p:cSld>
</p:sld>`

func TestAPptxReadsSlidesInNumericOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deck.pptx")
	writeZip(t, path, map[string]string{
		"ppt/slides/slide2.xml":  fmt.Sprintf(slideXMLTemplate, "second slide body"),
		"ppt/slides/slide1.xml":  fmt.Sprintf(slideXMLTemplate, "opening title"),
		"ppt/slides/slide10.xml": fmt.Sprintf(slideXMLTemplate, "closing remarks"),
	})
	text, err := ExtractText(path)
	if err != nil {
		t.Fatal(err)
	}
	opening := strings.Index(text, "opening title")
	second := strings.Index(text, "second slide body")
	closing := strings.Index(text, "closing remarks")
	if opening < 0 || second < 0 || closing < 0 {
		t.Fatalf("missing slide text in:\n%s", text)
	}
	// Numeric order, not lexicographic: slide10 sorts before slide2 as a
	// string, and reading it second would scramble the deck.
	if !(opening < second && second < closing) {
		t.Errorf("slides out of order in:\n%s", text)
	}
	for _, want := range []string{"slide 1", "slide 2", "slide 10"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q slide marker in:\n%s", want, text)
		}
	}
}

// buildPDF assembles a minimal two-page PDF around pre-compressed content
// streams. Offsets are real, because the object splitter works on positions,
// and a fixture with fake offsets would prove nothing.
func buildPDF(t *testing.T, streams [][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := []int{}
	add := func(body string) {
		offsets = append(offsets, out.Len())
		out.WriteString(body)
	}
	add("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	add("2 0 obj\n<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>\nendobj\n")
	add("3 0 obj\n<< /Type /Page /Parent 2 0 R /Contents 5 0 R >>\nendobj\n")
	add("4 0 obj\n<< /Type /Page /Parent 2 0 R /Contents 6 0 R >>\nendobj\n")
	for i, stream := range streams {
		var compressed bytes.Buffer
		writer := zlib.NewWriter(&compressed)
		if _, err := writer.Write(stream); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		add(fmt.Sprintf("%d 0 obj\n<< /Length %d /Filter /FlateDecode >>\nstream\n", 5+i, compressed.Len()))
		out.Write(compressed.Bytes())
		out.WriteString("\nendstream\nendobj\n")
	}
	xrefAt := out.Len()
	out.WriteString(fmt.Sprintf("xref\n0 %d\n", len(offsets)+1))
	out.WriteString("0000000000 65535 f \n")
	for _, offset := range offsets {
		out.WriteString(fmt.Sprintf("%010d 00000 n \n", offset))
	}
	out.WriteString(fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xrefAt))
	return out.Bytes()
}

func TestAPdfReadsBothPagesInOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.pdf")
	raw := buildPDF(t, [][]byte{
		[]byte("BT /F1 12 Tf 72 720 Td (Hello from page one) Tj ET"),
		[]byte("BT /F1 12 Tf 72 720 Td [(Good) -250 (bye from page two)] TJ ET"),
	})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	text, err := ExtractText(path)
	if err != nil {
		t.Fatal(err)
	}
	one := strings.Index(text, "Hello from page one")
	two := strings.Index(text, "Good bye from page two")
	if one < 0 || two < 0 {
		t.Fatalf("missing page text in:\n%s", text)
	}
	if one > two {
		t.Errorf("pages out of order in:\n%s", text)
	}
}

// A PDF with only an image stream and no text operators must fail loudly. An
// empty success would read as "the document says nothing", which is the one
// wrong answer here: the document says something, just not in extractable text.
func TestAScannedPdfFailsLoudlyInsteadOfEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scan.pdf")
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	out.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	out.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	out.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /Resources << /XObject << /Im1 4 0 R >> >> /Contents 5 0 R >>\nendobj\n")
	out.WriteString("4 0 obj\n<< /Type /XObject /Subtype /Image /Width 10 /Height 10 /Length 20 /Filter /DCTDecode >>\nstream\n0123456789abcdefghij\nendstream\nendobj\n")
	out.WriteString("5 0 obj\n<< /Length 11 >>\nstream\nq /Im1 Do Q\nendstream\nendobj\n")
	out.WriteString("trailer\n<< /Root 1 0 R >>\n")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractText(path); err == nil {
		t.Fatal("a scanned PDF extracted as empty success")
	} else if !strings.Contains(err.Error(), "OCR") {
		t.Fatalf("wrong refusal, must name the actual gap: %v", err)
	}
}

func TestANonPdfWithAPdfExtensionIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fake.pdf")
	if err := os.WriteFile(path, []byte("definitely not a pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractText(path); err == nil {
		t.Fatal("a renamed text file passed as PDF without an error")
	}
}

func TestSummarizeHeadsEveryFormat(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "b.zip")
	writeZip(t, archive, map[string]string{"a.txt": "hi"})
	docx := filepath.Join(dir, "m.docx")
	writeZip(t, docx, map[string]string{"word/document.xml": wordDocumentXML})
	pdf := filepath.Join(dir, "n.pdf")
	if err := os.WriteFile(pdf, buildPDF(t, [][]byte{[]byte("BT (page text) Tj ET")}), 0o644); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		archive: "unpacked to",
		docx:    "word",
		pdf:     "pdf",
	} {
		summary, err := Summarize(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(summary, filepath.Base(path)) || !strings.Contains(summary, want) {
			t.Errorf("summary of %s missing header:\n%s", path, summary)
		}
	}
}
