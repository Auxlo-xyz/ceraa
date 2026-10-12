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

const (
	// ArchiveKeep is how many compactions are retained per session. Each entry
	// holds the whole discarded head, which for a real session is a couple of
	// hundred kilobytes, so this is a small number on purpose. The newest is the
	// interesting one; the previous one is context for how the summary drifted.
	ArchiveKeep = 2

	// ArchiveMaxBytes caps one entry, so a pathological session cannot fill the
	// disk. Beyond this the head is dropped rather than truncated: a partial head
	// that looks whole is worse than none.
	ArchiveMaxBytes = 4 << 20
)

// ArchiveEntry is one compaction's discarded head, plus the summary it produced.
type ArchiveEntry struct {
	Seq       int                    `json:"seq"`
	At        time.Time              `json:"at"`
	Summary   string                 `json:"summary"`
	Discarded []sessionstore.Message `json:"discarded"`
	Truncated bool                   `json:"truncated,omitempty"`
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
func (c *Compactor) writeArchive(sessionID string, seq int, summary string, discarded []sessionstore.Message) {
	if c.ArchiveDir == "" || sessionID == "" || len(discarded) == 0 {
		return
	}
	entry := ArchiveEntry{
		Seq:       seq,
		At:        time.Now().UTC(),
		Summary:   summary,
		Discarded: redactArchiveMessages(discarded),
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if len(encoded) > ArchiveMaxBytes {
		entry.Truncated = true
		entry.Discarded = nil
		if encoded, err = json.Marshal(entry); err != nil {
			return
		}
	}

	path := archivePath(c.ArchiveDir, sessionID)
	lock, err := flock.Acquire(path + ".lock")
	if err != nil {
		return
	}
	defer lock.Release()
	archive := ArchiveFile{SessionID: sessionID}
	// A corrupt or absent sidecar must not stop a new one being written.
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &archive); err != nil {
			archive = ArchiveFile{SessionID: sessionID}
		}
		archive.SessionID = sessionID
	}
	archive.Entries = append(archive.Entries, entry)
	if len(archive.Entries) > ArchiveKeep {
		archive.Entries = archive.Entries[len(archive.Entries)-ArchiveKeep:]
	}
	out, err := json.Marshal(archive)
	if err != nil {
		return
	}
	writeFileAtomic(path, out)
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
func writeFileAtomic(path string, data []byte) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmpPath, path)
}
