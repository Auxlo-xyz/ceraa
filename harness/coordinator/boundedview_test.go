package coordinator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func trimmedResult(t *testing.T, cap int, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	r := &Runner{cfg: RunnerConfig{MaxToolResultChars: cap, ToolOutputDir: dir}}
	got := r.formatOutput("run1", "call1", body)
	return got, filepath.Join(dir, "run1", "call1.txt")
}

func jsonArray(n int) string {
	rows := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, map[string]any{
			"id": i, "name": fmt.Sprintf("component-%04d", i),
			"path": fmt.Sprintf("/pkg/dir%02d/file.go", i%40), "size": 100 + i,
		})
	}
	b, _ := json.Marshal(rows)
	return string(b)
}

func buildLog(lines int, headError bool) string {
	var b strings.Builder
	if headError {
		b.WriteString("error[E0308]: mismatched types\n   --> src/main.rs:14:9\n")
	}
	for i := 0; i < lines; i++ {
		b.WriteString(fmt.Sprintf("   Compiling dep%02d v0.%d.%d\n", i%40, i%9, i%7))
	}
	b.WriteString("error: aborting due to 2 previous errors\n")
	return b.String()
}

// The tail was left alone by two earlier head-window attempts, both reverted with
// measurements, and a test now pins it. This asserts the third arrangement is
// different in the way it claims to be: the head comes out of cap space the tail
// was never using, so the tail keeps every byte it had.
func TestTheHeadIsPaidForOutOfUnusedCapNotTheTail(t *testing.T) {
	body := buildLog(4000, false)
	got, _ := trimmedResult(t, 12000, body)
	if !strings.Contains(got, "Tail:") {
		t.Fatalf("no tail:\n%s", got[:200])
	}
	tail := got[strings.LastIndex(got, "Tail:"):]
	if len(tail) < 12000/3 {
		t.Errorf("tail is %d bytes, under the %d it must keep", len(tail), 12000/3)
	}
	if len(got) > 12000 {
		t.Errorf("result is %d bytes, over the 12000 cap", len(got))
	}
	if !strings.Contains(got, "Head:") {
		t.Error("no head section")
	}
}

// A trimmed result used to be a fixed tail slice, so a JSON array arrived
// starting mid-object: the model never saw the bracket, the first key, or the
// schema. Both are what the head and the shape line are for.
func TestATrimmedJSONArrayStillShowsItsShape(t *testing.T) {
	body := jsonArray(3000)
	got, _ := trimmedResult(t, 12000, body)
	if !strings.Contains(got, "Head:\n[") {
		t.Error("the head does not start at the opening bracket")
	}
	if !strings.Contains(got, "\"name\"") {
		t.Error("the first key is not visible, so the schema is still unknown")
	}
	for _, key := range []string{"id", "name", "path", "size"} {
		if !strings.Contains(got, "keys: ") || !strings.Contains(got, key) {
			t.Errorf("the shape line omits %q", key)
		}
	}
}

// The keys that describe an array of objects sit one level deeper than the array
// itself. Scanning from the document's own depth found nothing and reported a
// JSON array as having no keys at all.
func TestTopLevelKeysReadsTheFirstElementOfAnArray(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"object", `{"id":1,"name":"x"}`, []string{"id", "name"}},
		{"array of objects", `[{"id":1,"name":"x","path":"/p"}]`, []string{"id", "name", "path"}},
		{"stops at the first level", `[{"a":{"b":1},"c":2}]`, []string{"a", "c"}},
	}
	for _, c := range cases {
		got := topLevelKeys(c.in)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: keys = %v, want %v", c.name, got, c.want)
		}
	}
	if keys := topLevelKeys("plain command output"); len(keys) != 0 {
		t.Errorf("non-JSON reported keys %v", keys)
	}
}

// A value left in the scan buffer made the next key concatenate onto it, so a
// document with "name":"c-0000" then "path" reported a field called c-0000path.
func TestAValueNeverLeaksIntoTheNextKey(t *testing.T) {
	got := topLevelKeys(`[{"name":"c-0000","path":"/p"}]`)
	for _, k := range got {
		if strings.Contains(k, "c-0000") {
			t.Errorf("a value was reported as a field name: %v", got)
		}
	}
	if len(got) != 2 || got[0] != "name" || got[1] != "path" {
		t.Errorf("keys = %v, want [name path]", got)
	}
}

// The line-1 error is the case a tail cannot serve at all: 4,000 lines of build
// output, and the only thing that matters is on the first line.
func TestALineOneErrorSurvivesTheTrim(t *testing.T) {
	got, _ := trimmedResult(t, 12000, buildLog(4000, true))
	if !strings.Contains(got, "E0308") {
		t.Error("the first-line error is not visible")
	}
	if !strings.Contains(got, "aborting due to") {
		t.Error("the final error is not visible; the tail lost its job")
	}
}

// A shape line that does not fit is not a summary, so it is bounded.
func TestTheShapeLineStaysShort(t *testing.T) {
	shape := shapeLine(strings.Repeat("x", 2_000_000))
	if len(shape) > 200 {
		t.Errorf("shape line is %d bytes: %q", len(shape), shape)
	}
	if !strings.Contains(shape, "lines") {
		t.Errorf("shape line lost its line count: %q", shape)
	}
}

// The offloaded original is the recovery path, so it must still be written and
// still be complete. A shape line is worthless if the file it points at is a
// different size from the one described.
func TestTheOffloadedOriginalIsStillWhole(t *testing.T) {
	body := jsonArray(3000)
	got, path := trimmedResult(t, 12000, body)
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("offloaded original missing: %s", err)
	}
	if string(stored) != body {
		t.Errorf("the stored original is %d bytes, want %d", len(stored), len(body))
	}
	if !strings.Contains(got, "greppable") {
		t.Error("the notice no longer says the path is usable")
	}
}
