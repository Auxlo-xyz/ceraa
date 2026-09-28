package compaction

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/redact"
	"ceraa/harness/sessionstore"
)

func archiveCompactor(t *testing.T) (*Compactor, string, string) {
	t.Helper()
	dir := t.TempDir()
	c := &Compactor{Settings: DefaultSettings(), ContextLimit: MaxTotalTokens, ArchiveDir: dir}
	return c, dir, "tg-archive-test"
}

func headMessages() []sessionstore.Message {
	return []sessionstore.Message{
		{Role: "user", Content: "fix the index"},
		{Role: "assistant", ToolCalls: []sessionstore.ToolCallRef{
			{ID: "1", Name: "read", Args: `{"path":"/repo/types.ts"}`},
		}},
		{Role: "tool", ToolName: "read", ToolResult: "token is sk-live-ARCHIVESECRET123456"},
		{Role: "assistant", ToolCalls: []sessionstore.ToolCallRef{
			{ID: "2", Name: "write", Args: `{"path":"/repo/types.ts","content":"secret-value-here"}`},
		}},
	}
}

func TestArchiveKeepsTheDiscardedHead(t *testing.T) {
	c, dir, id := archiveCompactor(t)
	c.writeArchive(id, 125, "## Objective\nfix the index", headMessages())

	raw, err := os.ReadFile(filepath.Join(dir, id+".compacted.json"))
	if err != nil {
		t.Fatalf("no archive written: %v", err)
	}
	var got ArchiveFile
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("archive is not valid json: %v", err)
	}
	if len(got.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(got.Entries))
	}
	entry := got.Entries[0]
	if entry.Seq != 125 {
		t.Errorf("seq = %d, want 125", entry.Seq)
	}
	if !strings.Contains(entry.Summary, "fix the index") {
		t.Errorf("the summary is not archived alongside the head: %q", entry.Summary)
	}
	// The whole point: the discarded messages survive, structured, so the summary
	// can be checked against them later.
	if len(entry.Discarded) != 4 {
		t.Fatalf("discarded = %d messages, want 4", len(entry.Discarded))
	}
	var found bool
	for _, message := range entry.Discarded {
		for _, call := range message.ToolCalls {
			if call.Name == "write" && strings.Contains(call.Args, "types.ts") {
				found = true
			}
		}
	}
	if !found {
		t.Error("the write call and its path are not in the archive; an audit needs the tool, the path and the order")
	}
}

func TestArchiveRedactsStoredSecretValues(t *testing.T) {
	redact.ResetLiterals()
	redact.RegisterLiteral("sk-live-ARCHIVESECRET123456")
	t.Cleanup(redact.ResetLiterals)

	c, dir, id := archiveCompactor(t)
	c.writeArchive(id, 1, "summary", headMessages())

	raw, err := os.ReadFile(filepath.Join(dir, id+".compacted.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-live-ARCHIVESECRET123456") {
		t.Error("the archive holds a stored credential value in plaintext")
	}
	// Redaction must not destroy what an audit needs.
	if !strings.Contains(string(raw), "types.ts") {
		t.Error("redaction removed the paths, which is the part an audit actually reads")
	}
	// The value must still be visibly present, masked: an audit needs to see that a
	// credential was in play, not to read it.
	if !strings.Contains(string(raw), "token is") {
		t.Error("redaction removed the surrounding text, so the audit cannot see that a credential was involved")
	}
}

func TestArchiveRetainsOnlyTheNewestEntries(t *testing.T) {
	c, dir, id := archiveCompactor(t)
	for seq := 1; seq <= ArchiveKeep+3; seq++ {
		c.writeArchive(id, seq, "summary", headMessages())
	}
	raw, err := os.ReadFile(filepath.Join(dir, id+".compacted.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got ArchiveFile
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != ArchiveKeep {
		t.Fatalf("entries = %d, want %d", len(got.Entries), ArchiveKeep)
	}
	if last := got.Entries[len(got.Entries)-1].Seq; last != ArchiveKeep+3 {
		t.Errorf("newest seq = %d, want %d; the archive must keep the latest, not the oldest", last, ArchiveKeep+3)
	}
}

// A missing or corrupt sidecar must not stop a new one being written: the
// compaction has already succeeded by this point.
func TestArchiveSurvivesACorruptSidecar(t *testing.T) {
	c, dir, id := archiveCompactor(t)
	path := filepath.Join(dir, id+".compacted.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.writeArchive(id, 7, "summary", headMessages())
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("a corrupt sidecar stopped the write: %v", err)
	}
	var got ArchiveFile
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the rewritten sidecar is not valid json: %v", err)
	}
	if len(got.Entries) != 1 {
		t.Errorf("entries = %d, want 1", len(got.Entries))
	}
}

// Unset ArchiveDir means no side effect at all, so a caller with nowhere to put it
// behaves exactly as before.
func TestArchiveIsOffWhenUnconfigured(t *testing.T) {
	c := &Compactor{Settings: DefaultSettings(), ContextLimit: MaxTotalTokens}
	if c.ArchiveDir != "" {
		t.Fatal("archiving should be off by default")
	}
	c.writeArchive("id", 1, "summary", headMessages()) // must not panic or write
}

// The archive is a diagnostic, never an input. If anything read it back into a
// prompt, the thing it is meant to protect — the summary — would be changeable by
// a sidecar file.
func TestArchiveDoesNotReachThePrompt(t *testing.T) {
	previous := "## Objective\nold"
	// buildPrompt is the only path from archived content into a model request.
	prompt := buildPrompt(previous, "head text", "recent text")
	for _, leak := range []string{"discarded", ".compacted.json", "read_files"} {
		if strings.Contains(prompt, leak) {
			t.Errorf("the compaction prompt mentions %q; the archive must not be readable by the model", leak)
		}
	}
}
