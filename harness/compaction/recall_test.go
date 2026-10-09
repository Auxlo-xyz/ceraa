package compaction

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/sessionstore"
)

func recallArchiveFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	archive := ArchiveFile{
		SessionID: "ses",
		Entries: []ArchiveEntry{
			{
				Seq:     10,
				Summary: "## Objective\nFix the index.\n## Errors & Fixes\nThe build failed with exit 3.",
				Discarded: []sessionstore.Message{
					{Role: "user", Content: "fix the index"},
					{Role: "assistant", Content: "on it", ToolCalls: []sessionstore.ToolCallRef{{ID: "c1", Name: "read", Args: `{"path":"types.ts"}`}}},
					{Role: "tool", ToolCallID: "c1", ToolName: "read", ToolResult: "the index is a map"},
				},
			},
			{
				Seq:     20,
				Summary: "## Objective\nShip it.\n## Errors & Fixes\nNone.",
				Discarded: []sessionstore.Message{
					{Role: "user", Content: "ship it"},
				},
			},
		},
	}
	raw, err := json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ses.compacted.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSearchArchiveFindsNewestFirst(t *testing.T) {
	dir := recallArchiveFixture(t)
	out, err := SearchArchive(dir, "ses", "index")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "entry 0") {
		t.Errorf("the hit does not name its entry: %q", out)
	}
	if !strings.Contains(out, "Fix the index") {
		t.Errorf("the summary hit is missing: %q", out)
	}
	if !strings.Contains(out, "the index is a map") {
		t.Errorf("the discarded message hit is missing: %q", out)
	}
}

func TestSearchArchiveIsCaseInsensitive(t *testing.T) {
	dir := recallArchiveFixture(t)
	out, err := SearchArchive(dir, "ses", "BUILD FAILED")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "exit 3") {
		t.Errorf("case-insensitive search missed: %q", out)
	}
}

func TestSearchArchiveReportsNoMatchAndNoArchive(t *testing.T) {
	dir := recallArchiveFixture(t)
	out, err := SearchArchive(dir, "ses", "zebra")
	if err != nil {
		t.Fatal(err)
	}
	if out != "No match in the archived history." {
		t.Errorf("no-match reads %q", out)
	}
	out, err = SearchArchive(dir, "nobody", "index")
	if err != nil {
		t.Fatal(err)
	}
	if out != "No compactions archived for this session." {
		t.Errorf("missing archive reads %q", out)
	}
}

func TestSearchArchiveRejectsABadPattern(t *testing.T) {
	dir := recallArchiveFixture(t)
	if _, err := SearchArchive(dir, "ses", "(["); err == nil {
		t.Error("an invalid regex was accepted")
	}
}

func TestOpenArchiveRendersAnEntryWhole(t *testing.T) {
	dir := recallArchiveFixture(t)
	out, err := OpenArchive(dir, "ses", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Ship it.") || !strings.Contains(out, "ship it") {
		t.Errorf("the opened entry is incomplete: %q", out)
	}
}

func TestOpenArchiveRejectsOutOfRange(t *testing.T) {
	dir := recallArchiveFixture(t)
	if _, err := OpenArchive(dir, "ses", 2); err == nil {
		t.Error("entry 2 of 2 was accepted")
	}
	if _, err := OpenArchive(dir, "ses", -1); err == nil {
		t.Error("a negative entry was accepted")
	}
}

func TestOpenArchiveSaysWhenTheHeadWasDropped(t *testing.T) {
	dir := t.TempDir()
	archive := ArchiveFile{
		SessionID: "ses",
		Entries:   []ArchiveEntry{{Seq: 5, Summary: "## Objective\nShort.", Truncated: true}},
	}
	raw, err := json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ses.compacted.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := OpenArchive(dir, "ses", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "dropped for size") {
		t.Errorf("a dropped head is not marked: %q", out)
	}
}
