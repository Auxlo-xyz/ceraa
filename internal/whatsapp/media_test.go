// Media landing tests (media.go): traversal in source names cannot escape
// the files dir, oversized attachments are refused, and every saved file
// lands under the given dir - never straight into /tmp.
package whatsapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSizedFile creates a source attachment of exactly n bytes with
// deterministic content. Shared with bridge_test.go media cases.
func writeSizedFile(t *testing.T, dir, name string, n int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create source %s: %v", p, err)
	}
	const chunk = "0123456789abcdef"
	written := 0
	for written < n {
		want := n - written
		if want > len(chunk) {
			want = len(chunk)
		}
		if _, err := f.WriteString(chunk[:want]); err != nil {
			t.Fatalf("write source %s: %v", p, err)
		}
		written += want
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close source %s: %v", p, err)
	}
	return p
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return b
}

// assertInside fails when dest escapes dir.
func assertInside(t *testing.T, dir, dest string) {
	t.Helper()
	rel, err := filepath.Rel(dir, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Errorf("saved path %q escapes files dir %q", dest, dir)
	}
}

func TestSaveInboundMediaRoundTrip(t *testing.T) {
	work := t.TempDir()
	src := writeSizedFile(t, work, "photo.JPG", 128)
	filesDir := filepath.Join(work, "wa-media")

	dest, err := SaveInboundMedia(InboundMessage{ID: "msg-1", MediaPath: src}, filesDir)
	if err != nil {
		t.Fatalf("SaveInboundMedia = %v, want nil", err)
	}
	assertInside(t, filesDir, dest)
	if filepath.Dir(dest) != filesDir {
		t.Errorf("saved to %q, want directly under %q", dest, filesDir)
	}
	// Extension comes from the source base, lowercased.
	if !strings.HasSuffix(dest, ".jpg") {
		t.Errorf("saved path %q keeps no normalized .jpg extension", dest)
	}
	if got, want := readFile(t, dest), readFile(t, src); string(got) != string(want) {
		t.Error("saved bytes differ from the source attachment")
	}
}

func TestSaveInboundMediaNeedsAttachmentAndID(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveInboundMedia(InboundMessage{ID: "msg-1"}, dir); err == nil {
		t.Error("empty MediaPath accepted, want an error")
	}
	src := writeSizedFile(t, dir, "a.bin", 8)
	if _, err := SaveInboundMedia(InboundMessage{MediaPath: src}, dir); err == nil {
		t.Error("empty message ID accepted, want an error")
	}
	if _, err := SaveInboundMedia(InboundMessage{ID: "..", MediaPath: src}, dir); err == nil {
		t.Error("dot-only message ID accepted, want an error")
	}
}

// Traversal-laden ids and source names must either be refused or land
// sanitized inside the files dir - never outside it.
func TestSaveInboundMediaTraversalStaysInside(t *testing.T) {
	work := t.TempDir()
	src := writeSizedFile(t, work, "x", 32)
	filesDir := filepath.Join(work, "wa-media")
	ids := []string{"../../x", "/abs/path", "a/b/c", "..\\..\\win", "...."}
	for _, id := range ids {
		dest, err := SaveInboundMedia(InboundMessage{ID: id, MediaPath: src}, filesDir)
		if err != nil {
			continue
		}
		assertInside(t, filesDir, dest)
	}
	// A traversal source path that does not exist is a plain refusal.
	if _, err := SaveInboundMedia(InboundMessage{ID: "ok-1", MediaPath: "../../x"}, filesDir); err == nil {
		t.Error("traversal source ../../x accepted, want an error")
	}
	entries, err := os.ReadDir(filesDir)
	if err != nil {
		t.Fatalf("read files dir: %v", err)
	}
	for _, e := range entries {
		assertInside(t, filesDir, filepath.Join(filesDir, e.Name()))
	}
}

func TestSaveInboundMediaEnforcesSizeCap(t *testing.T) {
	work := t.TempDir()
	big := writeSizedFile(t, work, "big.bin", MaxInboundMediaBytes+100)
	filesDir := filepath.Join(work, "wa-media")

	_, err := SaveInboundMedia(InboundMessage{ID: "big-1", MediaPath: big}, filesDir)
	if err == nil {
		t.Fatalf("oversized attachment accepted, want refusal past %d bytes", MaxInboundMediaBytes)
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("oversize error = %v, want it to name the size bound", err)
	}
	// The refused copy must not linger as a partial file.
	if entries, _ := os.ReadDir(filesDir); len(entries) != 0 {
		t.Errorf("refused save left %d files behind", len(entries))
	}
}

func TestSaveInboundMediaAtCapBoundary(t *testing.T) {
	work := t.TempDir()
	exact := writeSizedFile(t, work, "exact.bin", MaxInboundMediaBytes)
	filesDir := filepath.Join(work, "wa-media")

	dest, err := SaveInboundMedia(InboundMessage{ID: "exact-1", MediaPath: exact}, filesDir)
	if err != nil {
		t.Fatalf("attachment at exactly the cap refused: %v", err)
	}
	if fi, err := os.Stat(dest); err != nil || fi.Size() != MaxInboundMediaBytes {
		t.Errorf("saved boundary file stat = (%v, %v), want %d bytes intact", dest, err, MaxInboundMediaBytes)
	}
}

// The regression this pins: uploads once went to a hardcoded /tmp dir that
// reboots wiped. The saved path must sit under the given dir even when the
// test temp root itself lives under /tmp.
func TestSaveInboundMediaNeverLandsOutsideGivenDir(t *testing.T) {
	work := t.TempDir()
	src := writeSizedFile(t, work, "note.txt", 16)
	filesDir := filepath.Join(work, "wa-media")

	dest, err := SaveInboundMedia(InboundMessage{ID: "n-1", MediaPath: src}, filesDir)
	if err != nil {
		t.Fatalf("SaveInboundMedia = %v", err)
	}
	assertInside(t, filesDir, dest)
	if filepath.Dir(dest) == os.TempDir() {
		t.Errorf("saved path %q sits directly in /tmp, want it under the files dir", dest)
	}
}

func TestSaveInboundMediaExtensionPolicy(t *testing.T) {
	work := t.TempDir()
	filesDir := filepath.Join(work, "wa-media")
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"uppercase lowered", "P.JPG", ".jpg"},
		{"mixed case lowered", "clip.Mp4", ".mp4"},
		{"no extension kept bare", "noext", ""},
		{"hostile extension dropped", "evil.%%%", ""},
		{"long extension dropped", "f.abcdefghijklm", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := writeSizedFile(t, work, tc.src, 16)
			dest, err := SaveInboundMedia(InboundMessage{ID: "ext-" + strings.ReplaceAll(tc.src, ".", "_"), MediaPath: src}, filesDir)
			if err != nil {
				t.Fatalf("SaveInboundMedia = %v", err)
			}
			assertInside(t, filesDir, dest)
			if tc.want == "" {
				if ext := filepath.Ext(dest); ext != "" {
					t.Errorf("saved path %q keeps extension %q, want none", dest, ext)
				}
			} else if !strings.HasSuffix(dest, tc.want) {
				t.Errorf("saved path %q lacks extension %q", dest, tc.want)
			}
		})
	}
}

func TestSaveInboundMediaMissingSource(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveInboundMedia(InboundMessage{
		ID:        "gone-1",
		MediaPath: filepath.Join(dir, "does-not-exist.jpg"),
	}, dir); err == nil {
		t.Error("missing source accepted, want an error")
	}
}

func TestRefuseEscapeDirectly(t *testing.T) {
	dir := t.TempDir()
	if err := refuseEscape(dir, filepath.Join(dir, "wa-x.jpg")); err != nil {
		t.Errorf("in-dir dest refused: %v", err)
	}
	if err := refuseEscape(dir, filepath.Join(dir, "..", "evil.jpg")); err == nil {
		t.Error("escaping dest accepted, want a refusal")
	}
	if err := refuseEscape(dir, "/etc/hostname"); err == nil {
		t.Error("absolute outside dest accepted, want a refusal")
	}
}

// Outbound media is phase two: SendMedia must name the gap, not fail
// silently or pretend to send.
func TestSendMediaNamesTheGap(t *testing.T) {
	if err := SendMedia(t.Context(), "chat@s.whatsapp.net", "/tmp/x.jpg"); err == nil {
		t.Error("SendMedia succeeded with no send-media path, want the gap named")
	}
}
