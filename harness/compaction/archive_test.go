package compaction

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/llm"
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
	c.writeArchive(id, ArchiveEntry{Seq: 125, Summary: "## Objective\nfix the index", Discarded: headMessages()})

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
	c.writeArchive(id, ArchiveEntry{Seq: 1, Summary: "summary", Discarded: headMessages()})

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

// Every entry is kept. This used to retain only the newest ArchiveKeep, which
// meant the third fold in a session silently destroyed the first fold's only
// copy of the messages it had discarded - the copy exists nowhere else, because
// the live transcript no longer holds them either.
func TestArchiveRetainsEveryEntry(t *testing.T) {
	c, dir, id := archiveCompactor(t)
	const folds = 7
	for seq := 1; seq <= folds; seq++ {
		c.writeArchive(id, ArchiveEntry{Seq: seq, Summary: "summary", Discarded: headMessages()})
	}
	raw, err := os.ReadFile(filepath.Join(dir, id+".compacted.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got ArchiveFile
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != folds {
		t.Fatalf("entries = %d, want %d; an old fold's discarded messages were deleted", len(got.Entries), folds)
	}
	// Oldest first, so a reader can address an entry by a stable index.
	for i, entry := range got.Entries {
		if entry.Seq != i+1 {
			t.Fatalf("entry %d has seq %d; order changed and the recall tool addresses entries by index", i, entry.Seq)
		}
		if len(entry.Discarded) == 0 {
			t.Fatalf("entry %d kept its summary but lost the messages it discarded", i)
		}
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
	c.writeArchive(id, ArchiveEntry{Seq: 7, Summary: "summary", Discarded: headMessages()})
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
	c.writeArchive("id", ArchiveEntry{Seq: 1, Summary: "summary", Discarded: headMessages()}) // must not panic or write
}

// The archive is a diagnostic, never an input. If anything read it back into a
// request, the thing it is meant to protect (the summary) would be changeable by
// a sidecar file. The only reader is the appender in archive.go, which carries
// old entries forward to disk; the instruction below is the only
// compaction-authored text that reaches a model.
func TestArchiveDoesNotReachThePrompt(t *testing.T) {
	instruction := compactionInstruction(1, nil)
	for _, leak := range []string{"discarded", ".compacted.json", "read_files"} {
		if strings.Contains(instruction, leak) {
			t.Errorf("the compaction instruction mentions %q; the archive must not be readable by the model", leak)
		}
	}
}

// A fold entry must carry what the turn cost. The summary says what happened;
// the stats say what the happening cost. Without them a compaction is only
// readable, not accountable - you cannot tell one that saved money from one
// that spent 200k tokens to save them, which is the comparison that decides
// whether the trigger threshold is set right.
func TestAFoldEntryCarriesTheTurnCost(t *testing.T) {
	c, dir, id := archiveCompactor(t)
	sess := &sessionstore.Session{ID: id, Runs: []sessionstore.Run{{
		ID: "run-9", Step: 3, Requests: 11,
		Usage: llm.UsageInfo{
			InputTokens: 900, OutputTokens: 120,
			CachedInputTokens: 80000, CacheWriteTokens: 4000,
		},
		Cost:         llm.CostInfo{TotalUSD: 1.25, Known: true},
		ProviderHost: "opencode.ai",
		PricingModel: "openai/gpt-5.6-luna",
	}}}

	stats := archiveStats(sess, 355)
	stats.MessagesAfter = 147
	c.writeArchive(id, ArchiveEntry{
		Kind: ArchiveFold, Seq: 355, Summary: "summary",
		Discarded: headMessages(), Stats: stats,
	})

	var got ArchiveFile
	raw, err := os.ReadFile(filepath.Join(dir, id+".compacted.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(got.Entries))
	}
	e := got.Entries[0]
	if e.Kind != ArchiveFold {
		t.Errorf("kind = %q, want %q", e.Kind, ArchiveFold)
	}
	if e.Stats.RunID != "run-9" || e.Stats.Step != 3 || e.Stats.Requests != 11 {
		t.Errorf("run identity lost: %+v", e.Stats)
	}
	if e.Stats.CachedInputTokens != 80000 || e.Stats.CacheWriteTokens != 4000 {
		t.Errorf("cache counts lost: %+v", e.Stats)
	}
	if e.Stats.InputTokens != 900 || e.Stats.OutputTokens != 120 {
		t.Errorf("token counts lost: %+v", e.Stats)
	}
	if e.Stats.CostUSD != 1.25 || !e.Stats.CostKnown {
		t.Errorf("cost lost or untrusted-but-unmarked: %+v", e.Stats)
	}
	if e.Stats.MessagesBefore != 355 || e.Stats.MessagesAfter != 147 {
		t.Errorf("transcript sizes lost: %+v", e.Stats)
	}
	if e.Stats.ProviderHost != "opencode.ai" || e.Stats.Model != "openai/gpt-5.6-luna" {
		t.Errorf("route attribution lost: %+v", e.Stats)
	}
	// The messages themselves must survive alongside the numbers.
	if len(e.Discarded) != len(headMessages()) {
		t.Errorf("discarded messages = %d, want %d", len(e.Discarded), len(headMessages()))
	}
}

// A tier merge deletes no messages. What it deletes is the set of summaries it
// folded into one, and that is the only thing the entry has to carry.
func TestAMergeEntryKeepsTheAbsorbedSummaries(t *testing.T) {
	c, dir, id := archiveCompactor(t)
	absorbed := []string{"## Objective\nship the parser", "## State\ntests green"}
	c.writeArchive(id, ArchiveEntry{
		Kind: ArchiveMerge, Seq: 10, Summary: "merged",
		Absorbed: absorbed, Stats: archiveStats(nil, 10),
	})

	var got ArchiveFile
	raw, err := os.ReadFile(filepath.Join(dir, id+".compacted.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	e := got.Entries[0]
	if e.Kind != ArchiveMerge {
		t.Errorf("kind = %q, want %q", e.Kind, ArchiveMerge)
	}
	if len(e.Absorbed) != 2 || e.Absorbed[0] != absorbed[0] || e.Absorbed[1] != absorbed[1] {
		t.Errorf("absorbed summaries = %v, want %v", e.Absorbed, absorbed)
	}
	if len(e.Discarded) != 0 {
		t.Errorf("a merge discarded %d messages; a merge discards summaries, not messages", len(e.Discarded))
	}
}

// An absent discarded set must stay absent rather than becoming an empty list,
// or every merge entry would claim to have discarded nothing that reads as
// something.
func TestAMergeEntryHasNoEmptyDiscardedList(t *testing.T) {
	c, dir, id := archiveCompactor(t)
	c.writeArchive(id, ArchiveEntry{Kind: ArchiveMerge, Seq: 1, Summary: "merged", Absorbed: []string{"one"}})
	raw, err := os.ReadFile(filepath.Join(dir, id+".compacted.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"discarded"`) {
		t.Errorf("a merge entry wrote an absent field as an empty list: %s", raw)
	}
}

// A session with no run has no run to attribute cost to. Recording zeros is the
// honest answer; borrowing the previous turn's numbers would be worse than none.
func TestArchiveStatsWithoutARunAreZeroNotGuessed(t *testing.T) {
	s := archiveStats(&sessionstore.Session{ID: "x"}, 42)
	if s.RunID != "" || s.InputTokens != 0 || s.CostKnown {
		t.Errorf("stats invented for a session with no runs: %+v", s)
	}
	if s.MessagesBefore != 42 {
		t.Errorf("the one thing known was lost: %+v", s)
	}
}
