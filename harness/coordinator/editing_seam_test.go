package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// The end-to-end path the model actually takes: JSON arguments arrive as text,
// are parsed, translated, dispatched, and applied to disk.
//
// The operation and registry tests each pin one side of that and would both stay
// green if the middle broke - an argument carried into the spec but dropped on
// the way to the operation, say. This goes through the same seam production does.
func TestAnOccurrenceReachesTheFileThroughTheWholeSeam(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("X\nkeep\nX\nkeep\nX\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{
		reg:   tool.DefaultRegistry(),
		store: store,
		opMgr: operation.New(nil),
		cfg:   RunnerConfig{SessionID: "occurrence", WorkDir: dir},
	}
	sess := &sessionstore.Session{ID: "occurrence", Version: 2}

	prepared, results, _, err := runner.prepareTools(context.Background(), sess, "run1", []llm.StreamedToolCall{{
		ID:        "edit1",
		Name:      "edit_line",
		Arguments: `{"path":"f.txt","old_text":"X","new_text":"Y","occurrence":3}`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 1 {
		t.Fatalf("prepared %d tools, declined results = %#v", len(prepared), results)
	}

	select {
	case result := <-runner.opMgr.Results():
		if result.Status != operation.StatusCompleted {
			t.Fatalf("edit reported %s: %s", result.Status, result.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout")
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The third X, not the first. If occurrence were dropped, uniqueness would
	// fail and nothing would be written; if it were ignored, the first X would
	// have been replaced. This content distinguishes those.
	if string(content) != "X\nkeep\nX\nkeep\nY\n" {
		t.Errorf("content = %q, want the third X replaced", content)
	}
}

// The failure message is the whole reason the change exists, so the model has to
// receive it through the same result channel, not only in a direct call.
func TestTheLineDiagnosticsReachTheModelThroughTheSeam(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("alpha\nx\nbeta\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{
		reg:   tool.DefaultRegistry(),
		store: store,
		opMgr: operation.New(nil),
		cfg:   RunnerConfig{SessionID: "diagnostics", WorkDir: dir},
	}
	sess := &sessionstore.Session{ID: "diagnostics", Version: 2}

	prepared, _, _, err := runner.prepareTools(context.Background(), sess, "run1", []llm.StreamedToolCall{{
		ID:        "edit1",
		Name:      "edit_line",
		Arguments: `{"path":"f.txt","old_text":"x","new_text":"y"}`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 1 {
		t.Fatalf("prepared %d tools", len(prepared))
	}

	select {
	case result := <-runner.opMgr.Results():
		if result.Status != operation.StatusFailed {
			t.Fatalf("a non-unique old_text reported %s", result.Status)
		}
		if !strings.Contains(result.Error, "lines 2, 4") {
			t.Errorf("error = %q, want the match lines named", result.Error)
		}
		if !strings.Contains(result.Error, "occurrence") {
			t.Errorf("error = %q, want it to say how to proceed", result.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout")
	}
}
