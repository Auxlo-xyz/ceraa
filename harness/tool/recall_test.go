package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/operation"
)

func writeTestArchive(t *testing.T, dir, session string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"session_id": session,
		"entries": []any{map[string]any{
			"seq":     10,
			"summary": "## Objective\nFix the index.",
			"discarded": []any{map[string]any{
				"role": "tool", "tool_call_id": "c1", "tool_name": "read",
				"tool_result": "the index is a map",
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, session+".compacted.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRecallArchiveNeedsAPatternOrAnEntry(t *testing.T) {
	tool := &RecallArchiveTool{}
	if _, err := tool.Translate(map[string]any{}); err == nil {
		t.Error("empty arguments were accepted")
	}
	if _, err := tool.Translate(map[string]any{"entry": -1}); err == nil {
		t.Error("a negative entry was accepted")
	}
	if _, err := tool.Translate(map[string]any{"entry": "first"}); err == nil {
		t.Error("a non-numeric entry was accepted")
	}
	spec, err := tool.Translate(map[string]any{"regex": "panic", "session": "ses"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Type != "recall_archive" || spec.Args["regex"] != "panic" || spec.Args["session"] != "ses" {
		t.Errorf("unexpected spec: %+v", spec)
	}
	spec, err = tool.Translate(map[string]any{"entry": 2.0})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Args["entry"] != 2 {
		t.Errorf("a float entry did not survive: %+v", spec.Args)
	}
}

func TestRecallArchiveRunSearchesAndOpens(t *testing.T) {
	dir := t.TempDir()
	writeTestArchive(t, dir, "ses")
	tool := &RecallArchiveTool{}
	out, err := tool.Run(operation.Spec{Type: "recall_archive", Args: map[string]any{
		"archive_dir": dir, "session_id": "ses", "regex": "map",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "entry 0") {
		t.Errorf("search missed the archived message: %q", out)
	}
	out, err = tool.Run(operation.Spec{Type: "recall_archive", Args: map[string]any{
		"archive_dir": dir, "session_id": "ses", "entry": 0,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "the index is a map") {
		t.Errorf("open missed the archived message: %q", out)
	}
}

func TestRecallArchiveRunFallsBackToTheCallingSession(t *testing.T) {
	dir := t.TempDir()
	writeTestArchive(t, dir, "ses")
	tool := &RecallArchiveTool{}
	out, err := tool.Run(operation.Spec{Type: "recall_archive", Args: map[string]any{
		"archive_dir": dir, "session_id": "ses", "regex": "map",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "No compactions archived") {
		t.Errorf("the injected session was not used: %q", out)
	}
}
