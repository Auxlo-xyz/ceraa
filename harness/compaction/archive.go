package compaction

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"ceraa/harness/flock"

	"ceraa/harness/redact"
	"ceraa/harness/sessionstore"
)

// Compaction deletes the messages it summarises. That is the point: the head is
// replaced by a summary so the context fits. But it also means the only record of
// what was actually said and done is the summary's own account of itself.
//
// That is unfalsifiable. There is no way to ask "did the summary keep the part
// that mattered", because the thing it would be compared against no longer exists.
// Every judgement about summary quality is therefore an impression.
//
// The archive keeps the discarded messages, structured rather than pre-digest, so
// the question becomes answerable later without this having to guess which
// questions those will be. It is written unconditionally and read only when
// asked: the recall tool pulls an entry on demand, the way a fetch pulls a
// page, and nothing here is ever injected into a request unasked. A failure to
// write it must never fail the compaction.

// ArchiveMaxBytes caps one entry, so a pathological session cannot fill the
// disk in a single write. Beyond this the head is dropped rather than
// truncated: a partial head that looks whole is worse than none. It says so
// when it happens, because the alternative is history that vanished quietly.
const ArchiveMaxBytes = 4 << 20

// ArchiveKind names which rewrite produced an entry.
const (
	// ArchiveFold is a compaction: the discarded head is gone from the
	// transcript and this is the only copy.
	ArchiveFold = "fold"
	// ArchiveMerge is a tier merge: the messages did not change, but the
	// summaries they produced were absorbed into one and are otherwise gone.
	ArchiveMerge = "merge"
)

// ArchiveStats is what the discarded turn cost, kept beside the messages it
// cost.
//
// A summary says what happened; this says what the happening cost. Without it a
// fold is only readable, not accountable: you cannot tell a compaction that
// saved money from one that spent 200k tokens to save them, which is the
// comparison that decides whether the threshold is set right.
type ArchiveStats struct {
	// RunID and Step identify the turn this fold happened inside.
	RunID string `json:"run_id,omitempty"`
	Step  int    `json:"step,omitempty"`
	// Model and ProviderHost attribute the cost to a route rather than to a
	// model name alone.
	Model        string `json:"model,omitempty"`
	ProviderHost string `json:"provider_host,omitempty"`
	// Requests is how many provider calls the run had made.
	Requests int `json:"requests,omitempty"`
	// The four token counts are the cache story: input is what was re-sent,
	// cached_input is what the provider already had.
	InputTokens       int `json:"input_tokens,omitempty"`
	OutputTokens      int `json:"output_tokens,omitempty"`
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  int `json:"cache_write_tokens,omitempty"`
	// CostUSD is the run total at the moment of the fold. Known is false when
	// the provider's usage could not be trusted, and then this is not money.
	CostUSD   float64 `json:"cost_usd,omitempty"`
	CostKnown bool    `json:"cost_known,omitempty"`
	UsageNote string  `json:"usage_note,omitempty"`
	// MessagesBefore and MessagesAfter are the transcript this fold produced.
	MessagesBefore int `json:"messages_before,omitempty"`
	MessagesAfter  int `json:"messages_after,omitempty"`
}

// ArchiveEntry is one compaction's discarded head, plus the summary it produced.
type ArchiveEntry struct {
	Kind      string                 `json:"kind,omitempty"`
	Seq       int                    `json:"seq"`
	At        time.Time              `json:"at"`
	Summary   string                 `json:"summary"`
	Discarded []sessionstore.Message `json:"discarded,omitempty"`
	// Absorbed is a merge's contribution: the summaries a tier merge folded
	// into one. A fold leaves it empty because a fold discards messages rather
	// than summaries.
	Absorbed  []string     `json:"absorbed,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
	Stats     ArchiveStats `json:"stats,omitempty"`
}

// ArchiveFile is the on-disk shape: the retained entries for one session.
type ArchiveFile struct {
	SessionID string         `json:"session_id"`
	Entries   []ArchiveEntry `json:"entries"`
}

// archivePath is the sidecar for a session, beside the session file itself.
func archivePath(dir, sessionID string) string {
	return filepath.Join(dir, sessionID+".compacted.json")
}

// writeArchive appends an entry, retaining only the newest ArchiveKeep.
//
// Best effort by design. The compaction has already succeeded by the time this is
// called, and losing the diagnostic is a smaller problem than failing the turn.
func (c *Compactor) writeArchive(sessionID string, entry ArchiveEntry) {
	if c.ArchiveDir == "" || sessionID == "" {
		return
	}
	if len(entry.Discarded) == 0 && len(entry.Absorbed) == 0 && entry.Summary == "" {
		return
	}
	if entry.Kind == "" {
		entry.Kind = ArchiveFold
	}
	if entry.At.IsZero() {
		entry.At = time.Now().UTC()
	}
	// Only redact a set that is actually there: redactArchiveMessages returns an
	// empty non-nil slice, which would rewrite an absent field as an empty list.
	if len(entry.Discarded) > 0 {
		entry.Discarded = redactArchiveMessages(entry.Discarded)
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		c.logf("archive: could not encode %s %d of session %s: %v", entry.Kind, entry.Seq, sessionID, err)
		return
	}
	if len(encoded) > ArchiveMaxBytes {
		// The head goes rather than being cut in half. A partial head that looks
		// whole is worse than none, and this says so instead of leaving a hole
		// nobody will notice.
		c.logf("archive: %s %d of session %s is %d bytes, over the %d cap; the discarded head was NOT saved",
			entry.Kind, entry.Seq, sessionID, len(encoded), ArchiveMaxBytes)
		entry.Truncated = true
		entry.Discarded = nil
		if encoded, err = json.Marshal(entry); err != nil {
			c.logf("archive: could not encode the trimmed %s %d of session %s: %v", entry.Kind, entry.Seq, sessionID, err)
			return
		}
	}

	path := archivePath(c.ArchiveDir, sessionID)
	lock, err := flock.Acquire(path + ".lock")
	if err != nil {
		c.logf("archive: could not lock %s: %v", path, err)
		return
	}
	defer lock.Release()
	archive := ArchiveFile{SessionID: sessionID}
	// A corrupt or absent sidecar must not stop a new one being written.
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &archive); err != nil {
			c.logf("archive: %s was unreadable and has been replaced: %v", path, err)
			archive = ArchiveFile{SessionID: sessionID}
		}
		archive.SessionID = sessionID
	}
	// No trimming. Every fold and every merge is kept, in order, for as long as
	// the session file exists. This used to keep only the newest ArchiveKeep
	// entries, which meant the third fold in a session silently destroyed the
	// first one's only copy of the discarded messages.
	archive.Entries = append(archive.Entries, entry)
	out, err := json.Marshal(archive)
	if err != nil {
		c.logf("archive: could not encode the archive for session %s: %v", sessionID, err)
		return
	}
	if err := writeFileAtomic(path, out); err != nil {
		c.logf("archive: could not write %s, so %s %d is not on disk: %v", path, entry.Kind, entry.Seq, err)
	}
}

// archiveStats reads what the turn being folded cost off the session's last run.
//
// The last run, not a search for "the current one". A fold can fire before the
// run has recorded any usage at all, and then the honest answer is a zero rather
// than the previous turn's numbers: an archive entry that misattributes cost to
// the wrong turn is worse than one that records none.
//
// CostKnown and UsageNote travel with the numbers because a total is not money
// unless the provider's counts described the requests, and a caller reading this
// months later has no other way to tell.
func archiveStats(sess *sessionstore.Session, before int) ArchiveStats {
	stats := ArchiveStats{MessagesBefore: before}
	if sess == nil || len(sess.Runs) == 0 {
		return stats
	}
	run := sess.Runs[len(sess.Runs)-1]
	stats.RunID = run.ID
	stats.Step = run.Step
	stats.Model = run.PricingModel
	stats.ProviderHost = run.ProviderHost
	stats.Requests = run.Requests
	stats.InputTokens = run.Usage.InputTokens
	stats.OutputTokens = run.Usage.OutputTokens
	stats.CachedInputTokens = run.Usage.CachedInputTokens
	stats.CacheWriteTokens = run.Usage.CacheWriteTokens
	stats.CostUSD = run.Cost.TotalUSD
	stats.CostKnown = run.Cost.Known
	stats.UsageNote = run.UsageTrust.Note
	return stats
}

// redactArchiveMessages scrubs stored credential values out of the archived copy.
//
// The archive is a new plaintext file, so it should not become a place a secret
// outlives its store. What survives redaction is the part an audit needs: which
// tool ran, on which path, in what order, and what it said about the work. A
// redacted value is still a value you can see was there.
func redactArchiveMessages(messages []sessionstore.Message) []sessionstore.Message {
	out := make([]sessionstore.Message, 0, len(messages))
	for _, message := range messages {
		copied := message
		copied.Content = redact.SecretsInText(copied.Content)
		copied.ToolResult = redact.SecretsInText(copied.ToolResult)
		copied.Reasoning = redact.SecretsInText(copied.Reasoning)
		if len(copied.ToolCalls) > 0 {
			calls := make([]sessionstore.ToolCallRef, 0, len(copied.ToolCalls))
			for _, call := range copied.ToolCalls {
				call.Args = redact.SecretsInText(call.Args)
				calls = append(calls, call)
			}
			copied.ToolCalls = calls
		}
		if copied.ToolCall != nil {
			call := *copied.ToolCall
			call.Args = redact.SecretsInText(call.Args)
			copied.ToolCall = &call
		}
		out = append(out, copied)
	}
	return out
}

// writeFileAtomic writes via a temporary file and a rename, so a crash mid-write
// leaves the previous sidecar intact rather than a half-written one.
//
// It returns its error rather than discarding it. This is the only thing standing
// between a fold and the one copy of the messages it discarded, so a caller that
// cannot see a failure here will report a retained history that is not on disk.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
