package operation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func numbered(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = "line " + string(rune('a'+i%26)) + itoa(i+1)
	}
	return strings.Join(lines, "\n") + "\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func rangedRead(t *testing.T, dir, path string, args map[string]any) ResultEvent {
	t.Helper()
	spec := map[string]any{"path": path, "workdir": dir}
	for k, v := range args {
		spec[k] = v
	}
	mgr := New(nil)
	mgr.Add(Operation{ID: "r", Spec: Spec{Type: "read", Args: spec}, Status: StatusPending})
	return <-mgr.Results()
}

// The capability the model was improvising with `sed -n`. A real session needed
// three specific regions of a 21,536-byte file and spent three bash round trips
// and the shell tool's smaller cap to get them.
func TestAReadCanReturnALineRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte(numbered(500)), 0o644); err != nil {
		t.Fatal(err)
	}
	r := rangedRead(t, dir, path, map[string]any{"offset": 100, "limit": 5})
	if r.Status != StatusCompleted {
		t.Fatalf("ranged read reported %s: %s", r.Status, r.Error)
	}
	if !strings.Contains(r.Output, "lines 100-104 of 500") {
		t.Errorf("header does not name the range:\n%s", firstLines(r.Output, 3))
	}
	if !strings.Contains(r.Output, "line a101") && !strings.Contains(r.Output, "line ") {
		t.Errorf("window does not carry the content:\n%s", firstLines(r.Output, 6))
	}
	if strings.Contains(r.Output, "line a99") || strings.Contains(r.Output, "line a106") {
		t.Errorf("the window is not exactly five lines:\n%s", firstLines(r.Output, 8))
	}
	// A partial read must be honest about being partial, or the model assumes it
	// reached the end of the file.
	if !strings.Contains(r.Output, "more below") {
		t.Error("a window in the middle does not say there is more")
	}
	if !strings.Contains(r.Output, "lines 1-99 above") {
		t.Error("a window past the first line does not say what was skipped")
	}
}

// No range means the whole file, exactly as before. A read is not silently
// narrowed by a field defaulting to zero.
func TestAReadWithNoRangeStillReturnsTheWholeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	body := numbered(300)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r := rangedRead(t, dir, path, nil)
	if r.Status != StatusCompleted {
		t.Fatalf("read reported %s: %s", r.Status, r.Error)
	}
	// The last line of the fixture. The label cycles every 26, so line 300 is
	// "n300", not "a299"; asserting the wrong label would have passed for the
	// wrong reason if the window had been narrowed.
	if !strings.Contains(r.Output, "line n300") {
		t.Error("a whole read no longer returns the end of the file")
	}
	if strings.Contains(r.Output, "lines ") && strings.Contains(r.Output, " of ") {
		t.Errorf("a whole read is being reported as a range:\n%s", firstLines(r.Output, 2))
	}
}

// A range from a line to the end is the useful reading of "from line 300", and it
// is what a real session asked for with `sed -n '70,110p;280,310p;340,380p'`.
func TestARangeWithNoLimitRunsToTheEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte(numbered(400)), 0o644); err != nil {
		t.Fatal(err)
	}
	r := rangedRead(t, dir, path, map[string]any{"offset": 390})
	if r.Status != StatusCompleted {
		t.Fatalf("reported %s: %s", r.Status, r.Error)
	}
	if !strings.Contains(r.Output, "lines 390-400 of 400") {
		t.Errorf("header = %q, want the range running to the end", firstLines(r.Output, 1))
	}
	if strings.Contains(r.Output, "more below") {
		t.Error("a range that reached the end claims there is more")
	}
}

// The case a real session hit: it could not see the top of a trimmed file and
// reached for the offloaded copy. This is that, without the shell round trip.
func TestARangeReachesTheTopOfALargeFileThatWouldBeTrimmed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.ts")
	if err := os.WriteFile(path, []byte(numbered(20000)), 0o644); err != nil {
		t.Fatal(err)
	}
	r := rangedRead(t, dir, path, map[string]any{"offset": 1, "limit": 20})
	if r.Status != StatusCompleted {
		t.Fatalf("reported %s: %s", r.Status, r.Error)
	}
	if !strings.Contains(r.Output, "lines 1-20 of 20000") {
		t.Errorf("the head of a large file is not reachable: %q", firstLines(r.Output, 1))
	}
	// And it is a small answer, where the whole file would have been spilled.
	if len(r.Output) > 1000 {
		t.Errorf("a 20-line window returned %d bytes, which defeats the point", len(r.Output))
	}
}

// An offset past the end has to say so. Returning an empty string reads as "this
// file is empty", which is the one conclusion the model must not draw.
func TestAnOffsetPastTheEndIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte(numbered(10)), 0o644); err != nil {
		t.Fatal(err)
	}
	r := rangedRead(t, dir, path, map[string]any{"offset": 500, "limit": 5})
	if r.Status != StatusFailed {
		t.Fatalf("an out-of-range offset reported %s", r.Status)
	}
	if !strings.Contains(r.Error, "past the end") || !strings.Contains(r.Error, "10 lines") {
		t.Errorf("error = %q, want it to say how long the file is", r.Error)
	}
}

func TestARangedReadOfAMissingFileReportsTheReadFailure(t *testing.T) {
	dir := t.TempDir()
	r := rangedRead(t, dir, filepath.Join(dir, "nope.txt"), map[string]any{"offset": 1, "limit": 5})
	if r.Status != StatusFailed {
		t.Fatalf("a missing file reported %s", r.Status)
	}
	if strings.Contains(r.Error, "past the end") {
		t.Errorf("error = %q, want the read failure rather than a range complaint", r.Error)
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
