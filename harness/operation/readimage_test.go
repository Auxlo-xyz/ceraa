package operation

import (
	"bytes"
	image2 "image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The seam under test: the model has no view_image tool, so a `read` that
// returns the file's bytes as text leaves it permanently unable to see a
// picture. A read of an image must attach the path instead.
//
// These tests go through the manager rather than calling the media helpers
// directly, because a helper that is never reached from a read does nothing.

// smallPNG writes a small valid PNG into dir and returns its path.
func smallPNG(t *testing.T, dir, name string) string {
	t.Helper()
	img := image2.NewRGBA(image2.Rect(0, 0, 24, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 24; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 10), G: uint8(y * 10), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A read of an image attaches it. The result text is a short one-line note,
// never the picture's bytes, which is what used to happen.
func TestAReadOfAnImageAttachesItInsteadOfTranscribingIt(t *testing.T) {
	dir := t.TempDir()
	shot := smallPNG(t, dir, "shot.png")

	result := runRead(t, shot)
	if result.Status != StatusCompleted {
		t.Fatalf("read failed: status=%q error=%q", result.Status, result.Error)
	}

	if len(result.Images) != 1 || result.Images[0] != shot {
		t.Fatalf("Images = %v, want exactly [%s]", result.Images, shot)
	}

	// The output is a note about the attachment, not the bytes.
	output := result.Output
	if !strings.Contains(output, "shot.png") || !strings.Contains(output, "attached as an image") {
		t.Errorf("Output = %q, want a one-line attachment note naming shot.png", output)
	}
	if strings.Contains(output, "\n") {
		t.Errorf("Output spans %d lines, want one line: %q", strings.Count(output, "\n"), output)
	}
	if strings.Contains(output, "PNG") || strings.Contains(output, "\x89PNG") {
		t.Errorf("Output carries raw PNG bytes, so the image was transcribed as text: %q", output)
	}
	if len(output) > 200 {
		t.Errorf("Output is %d bytes, want a short note: %q", len(output), output)
	}
	// The bytes on disk must still be there: the read attached, it did not move
	// or rewrite anything.
	raw, err := os.ReadFile(shot)
	if err != nil || len(raw) == 0 {
		t.Fatalf("the image is gone after the read: err=%v bytes=%d", err, len(raw))
	}
}

// A text file must still come back as text with no image attached, so the image
// path did not swallow ordinary reads.
func TestAReadOfATextFileIsUnaffected(t *testing.T) {
	dir := t.TempDir()
	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, []byte("first line\nsecond line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := runRead(t, notes)
	if result.Status != StatusCompleted {
		t.Fatalf("read failed: status=%q error=%q", result.Status, result.Error)
	}
	if len(result.Images) != 0 {
		t.Errorf("Images = %v, want none for a text file", result.Images)
	}
	if result.Output != "first line\nsecond line\n" {
		t.Errorf("Output = %q, want the file's text", result.Output)
	}
	if strings.Contains(result.Output, "attached as an image") {
		t.Errorf("a text file was reported as an image: %q", result.Output)
	}
}
