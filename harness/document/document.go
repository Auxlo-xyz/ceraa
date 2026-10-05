package document

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Kind is what a file is for extraction purposes. It comes from the extension,
// because Telegram preserves the sender's filename and every archive or office
// format here announces itself that way.
type Kind string

const (
	// KindArchive is a zip file: listed and unpacked, never read as text.
	KindArchive Kind = "archive"
	// KindPDF is a PDF document: text operators are extracted, images are not.
	KindPDF Kind = "pdf"
	// KindWord is a DOCX file, which is a zip of XML and is read as one.
	KindWord Kind = "word"
	// KindSlides is a PPTX file, read slide by slide in numeric order.
	KindSlides Kind = "slides"
)

// KindForPath names the extraction kind for a path, or false when the file is
// none of these and the caller should read it the ordinary way.
func KindForPath(path string) (Kind, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".zip":
		return KindArchive, true
	case ".pdf":
		return KindPDF, true
	case ".docx":
		return KindWord, true
	case ".pptx":
		return KindSlides, true
	}
	return "", false
}

const (
	// maxExtractedText caps what one document contributes, so a large manual
	// does not eat the context budget in a single read. The raw read path caps
	// at 5MB; extracted text is denser, so it caps lower.
	maxExtractedText = 1024 * 1024
	// maxArchiveFiles bounds how many members one archive can spill.
	maxArchiveFiles = 1000
	// maxArchiveTotal bounds the uncompressed bytes written for one archive.
	// A zip that claims gigabytes from kilobytes is a bomb, and the per-member
	// counter below is what catches it rather than trusting the header.
	maxArchiveTotal = 64 * 1024 * 1024
	// maxArchiveMember bounds a single member for the same reason.
	maxArchiveMember = 16 * 1024 * 1024
)

// ExtractText returns the readable text of a PDF, DOCX or PPTX file. Archives
// go through Summarize instead, because their content is a listing plus paths
// the model can read next, not a body of text.
func ExtractText(path string) (string, error) {
	kind, ok := KindForPath(path)
	if !ok {
		return "", fmt.Errorf("%s is not a document this package reads", filepath.Base(path))
	}
	var text string
	var err error
	switch kind {
	case KindPDF:
		text, err = pdfText(path)
	case KindWord:
		text, err = officeText(path, func([]string) []string { return []string{wordMain} }, wordHeadersFooters)
	case KindSlides:
		text, err = officeText(path, slidesMain, nil)
	default:
		return "", fmt.Errorf("%s is an archive; Summarize lists it", filepath.Base(path))
	}
	if err != nil {
		return "", err
	}
	return capText(text), nil
}

// Summarize renders a document for the model: a one-line header naming the
// source and what was found, then the content. For an archive the content is
// the member listing with the extracted paths, so the next read is obvious.
func Summarize(path string) (string, error) {
	kind, ok := KindForPath(path)
	if !ok {
		return "", fmt.Errorf("%s is not a document this package reads", filepath.Base(path))
	}
	if kind == KindArchive {
		return summarizeArchive(path)
	}
	text, err := ExtractText(path)
	if err != nil {
		return "", err
	}
	unit := map[Kind]string{KindPDF: "pages", KindWord: "paragraphs", KindSlides: "slides"}[kind]
	return fmt.Sprintf("%s (%s, %s of text):\n\n%s", filepath.Base(path), kind, unit, text), nil
}

// capText bounds extracted text. Truncation lands on a UTF-8 boundary and says
// so, because silently cutting mid-rune corrupts the tail for no saving.
func capText(text string) string {
	if len(text) <= maxExtractedText {
		return text
	}
	cut := maxExtractedText
	for cut > 0 && !validPrefix(text, cut) {
		cut--
	}
	return text[:cut] + "\n...[extracted text truncated]..."
}

func validPrefix(text string, cut int) bool {
	for i := cut - 1; i >= 0 && i >= cut-4; i-- {
		c := text[i]
		if c < 0x80 || c >= 0xC0 {
			break
		}
		if i == cut-1 {
			return false
		}
	}
	return true
}

// openOffice opens a DOCX or PPTX file as the zip it is. The caller names which
// inner file holds the text, because word and slides keep it in different
// places but are otherwise the same trick: XML in a zip.
func openOffice(path string) (*zip.ReadCloser, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory; name a file", filepath.Base(path))
	}
	return zip.OpenReader(path)
}

// officeFileText reads one XML file out of an open office zip.
func officeFileText(zr *zip.ReadCloser, name string, collect func(xmlText []byte) string) (string, error) {
	for _, file := range zr.File {
		if file.Name != name {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return "", err
		}
		raw, err := io.ReadAll(io.LimitReader(rc, maxExtractedText*4))
		_ = rc.Close()
		if err != nil {
			return "", err
		}
		return collect(raw), nil
	}
	return "", fmt.Errorf("no %s in this document", name)
}

// ArchiveMember describes one extracted member for the listing.
type ArchiveMember struct {
	// Path is where the member now lives on disk, ready for a read.
	Path string
	// Size is the uncompressed bytes written.
	Size int64
	// Skipped names why a member was left out: nested archives are listed but
	// not expanded, and anything unsafe is refused rather than cleaned.
	Skipped string
}

// ExtractArchive unpacks a zip next to itself into "<name>.extracted" and
// returns the members. The destination is deterministic so a second read of
// the same archive reuses it instead of unpacking again, checked by a marker
// holding the archive's size and mtime.
func ExtractArchive(path string) ([]ArchiveMember, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory; name a file", filepath.Base(path))
	}
	dest := strings.TrimSuffix(path, filepath.Ext(path)) + ".extracted"
	marker := filepath.Join(dest, ".ceraa-extracted")
	want := fmt.Sprintf("%d\n%d\n", info.Size(), info.ModTime().Unix())
	if raw, readErr := os.ReadFile(marker); readErr == nil && string(raw) == want {
		return listExtracted(dest)
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer zr.Close()
	if len(zr.File) > maxArchiveFiles {
		return nil, fmt.Errorf("%s holds %d files, over the %d limit", filepath.Base(path), len(zr.File), maxArchiveFiles)
	}
	_ = os.RemoveAll(dest)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, err
	}
	var members []ArchiveMember
	var total int64
	for _, file := range zr.File {
		member, wrote, skipErr := extractMember(file, dest, total)
		if skipErr != nil {
			_ = os.RemoveAll(dest)
			return nil, skipErr
		}
		total += wrote
		members = append(members, member)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Path < members[j].Path })
	if err := os.WriteFile(marker, []byte(want), 0o644); err != nil {
		_ = os.RemoveAll(dest)
		return nil, err
	}
	return members, nil
}

// extractMember writes one zip member, or refuses it with a reason. Refusals
// are per member and reported, never fatal: one hostile path must not cost the
// rest of the archive, and silently dropping it would lie about the contents.
func extractMember(file *zip.File, dest string, total int64) (ArchiveMember, int64, error) {
	name := file.Name
	member := ArchiveMember{}
	// A zip-slip path escapes the destination, and cleaning it would keep a
	// hostile entry under a name nobody recognises. Refuse it and say so. The
	// check runs on the raw name before cleaning, because cleaning resolves
	// ".." away and there would be nothing left to catch.
	slash := strings.ReplaceAll(name, "\\", "/")
	clean := filepath.Clean(slash)
	if filepath.IsAbs(slash) || clean == ".." || strings.HasPrefix(clean, "../") {
		member.Path = name
		member.Skipped = "unsafe path, not extracted"
		return member, 0, nil
	}
	rel := strings.TrimPrefix(clean, "/")
	if rel == "" || strings.HasSuffix(rel, "/") {
		return member, 0, nil
	}
	target := filepath.Join(dest, rel)
	member.Path = target
	// A symlink or device node in an archive is never what the model asked
	// for. Extracting one would put filesystem objects where plain files were
	// expected, so they are listed and left alone.
	if file.Mode()&os.ModeSymlink != 0 || !file.Mode().IsRegular() {
		member.Skipped = "not a regular file, not extracted"
		return member, 0, nil
	}
	// A nested archive is a listing entry, not a recursion. Expanding them
	// would multiply the bomb surface for a convenience nobody asked for yet.
	if strings.HasSuffix(strings.ToLower(rel), ".zip") {
		member.Skipped = "nested archive, listed but not expanded"
		return member, 0, nil
	}
	if file.UncompressedSize64 > maxArchiveMember {
		member.Skipped = fmt.Sprintf("over the %d MB per-file limit", maxArchiveMember/1024/1024)
		return member, 0, nil
	}
	rc, err := file.Open()
	if err != nil {
		member.Skipped = "unreadable: " + err.Error()
		return member, 0, nil
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return member, 0, err
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return member, 0, err
	}
	// The header size is a claim, not a fact. Count the bytes actually written
	// and stop when the archive-wide budget is spent, because a lying header
	// is exactly how a bomb works.
	remaining := maxArchiveTotal - total
	wrote, copyErr := io.Copy(out, io.LimitReader(rc, remaining+1))
	closeErr := out.Close()
	if copyErr != nil {
		return member, 0, copyErr
	}
	if closeErr != nil {
		return member, 0, closeErr
	}
	if wrote > remaining {
		_ = os.Remove(target)
		_ = os.RemoveAll(dest)
		return member, 0, fmt.Errorf("archive is over the %d MB total limit", maxArchiveTotal/1024/1024)
	}
	member.Size = wrote
	return member, wrote, nil
}

// listExtracted re-reads a previous extraction for the reuse path.
func listExtracted(dest string) ([]ArchiveMember, error) {
	var members []ArchiveMember
	err := filepath.WalkDir(dest, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || d.Name() == ".ceraa-extracted" {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		members = append(members, ArchiveMember{Path: path, Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Path < members[j].Path })
	return members, nil
}

func summarizeArchive(path string) (string, error) {
	members, err := ExtractArchive(path)
	if err != nil {
		return "", err
	}
	var kept, skipped int
	var total int64
	var lines []string
	for _, m := range members {
		if m.Skipped != "" {
			skipped++
			lines = append(lines, fmt.Sprintf("- %s (%s)", filepath.Base(m.Path), m.Skipped))
			continue
		}
		kept++
		total += m.Size
		lines = append(lines, fmt.Sprintf("- %s (%s)", m.Path, humanBytes(m.Size)))
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%s unpacked to %s (%d files, %s",
		filepath.Base(path),
		strings.TrimSuffix(path, filepath.Ext(path))+".extracted",
		kept, humanBytes(total))
	if skipped > 0 {
		fmt.Fprintf(&out, ", %d skipped", skipped)
	}
	out.WriteString("):\n")
	out.WriteString(strings.Join(lines, "\n"))
	out.WriteString("\nRead a member path to see its contents.")
	return out.String(), nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/1024/1024)
	case n >= 1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
