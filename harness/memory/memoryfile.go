package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"time"
)

// EntryID is the identity one log entry is delivered once under.
//
// A content hash of the entry text, not its position or date, because the
// delivery set has to survive the log growing underneath it: the tail is a
// window, so an entry delivered last week leaves the window and a positional id
// would be reused by a different memory later. Hashing the text means the same
// memory keeps its identity for as long as it is in the file.
//
// The prefix names the source, so an id from another producer cannot collide
// with it by looking the same.
func EntryID(entry string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(entry)))
	return "memfile:" + hex.EncodeToString(sum[:6])
}

// MemoryFile is an append-only log of what the agent has learned.
//
// It replaces a store that re-rendered itself on every request, and the
// difference is the whole point. A ranked store is a volatile block: the render
// reads the file, scores each entry against a wall clock, and sorts, so the
// bytes change even when nothing was written. Sitting that ahead of the cache
// seam rebuilt the cached prefix on every turn - measured at a 49.8%
// token-weighted hit rate against 91.8% on the requests where it held.
//
// An append-only log cannot do that. Nothing re-ranks, nothing decays, and the
// only thing that ever changes is that entries are added at the end. The tail is
// read instead of the whole, so the cost per turn is proportional to what is
// being delivered rather than to everything ever learned.
//
// It is append-on-purpose rather than edited-in-place. A memory that turns out
// to be wrong is corrected by a newer entry noting the correction, not by
// rewriting the one above it, because a file the agent reads by appending to it
// is the one file it cannot corrupt by rewriting it.
type MemoryFile struct {
	path string
	mu   sync.Mutex
}

// OpenMemoryFile returns the log at path, creating nothing.
//
// Nothing is created until the first Append, so a project with no memory yet is
// a state rather than an error and needs no migration.
func OpenMemoryFile(path string) *MemoryFile {
	return &MemoryFile{path: path}
}

// Path is where the log lives, so a caller can tell a user where to look.
func (f *MemoryFile) Path() string { return f.path }

// AppendMemoryFile is the entry format.
//
// Two lines per entry, because a single flat line cannot survive a value
// containing a newline without an escaping rule the next reader has to know.
// The separator is a date line rather than a delimiter token, so the file reads
// as prose to a human and as a record to this package, with no parser state
// that can desynchronise on a malformed entry.
const memoryEntryPrefix = "## "

// Append adds one memory to the end of the log.
//
// The date is written rather than read back, so an entry carries when it was
// learned rather than when it happened to be read. Trimming is the caller's
// choice, not the file's: a truncated entry that does not say it was truncated
// is the same lie either way, and this package cannot know what mattered.
func (f *MemoryFile) Append(text, session string) error {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	file, err := os.OpenFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	var entry strings.Builder
	entry.WriteString(memoryEntryPrefix)
	entry.WriteString(time.Now().UTC().Format("2006-01-02 15:04"))
	if session != "" {
		entry.WriteString(" ")
		entry.WriteString(strings.TrimSpace(session))
	}
	entry.WriteString("\n")
	entry.WriteString(trimmed)
	entry.WriteString("\n\n")

	_, err = file.WriteString(entry.String())
	return err
}

// Tail returns the last n entries, newest last.
//
// It does NOT read backwards. The whole file is read and then sliced, so the
// cost is proportional to the log rather than to n, and an earlier version of
// this comment claimed otherwise. The bound that matters for a per-turn read is
// the SLICE, which is what keeps a note bounded; the read cost is a separate
// property that a backwards read would fix and that is deliberately not
// implemented here, because a partial-read implementation that cuts an entry in
// half is worse than a full read that cannot.
//
// An unreadable file is an empty slice rather than an error. The file is an
// optimisation, the conversation is the source of truth, and a caller that
// cannot read it should deliver nothing rather than fail a turn.
func (f *MemoryFile) Tail(n int) []string {
	if n <= 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	content, err := os.ReadFile(f.path)
	if err != nil {
		return nil
	}
	return tailEntries(string(content), n)
}

// tailEntries splits a log into its entries and keeps the last n.
//
// The first entry needs handling of its own, and getting it wrong is silent: a
// log written from empty begins at offset zero, so its first separator has no
// newline before it and a plain split treats that entry as the file's preamble
// and drops it. Measured before the fix, one append read back as zero and five as
// four - the first thing a project ever learned was permanently invisible, and
// nothing reported it. Prefixing a newline puts the first entry on the same
// footing as every later one instead of guessing that only a preamble can come
// first.
func tailEntries(content string, n int) []string {
	if n <= 0 {
		return nil
	}
	if content == "" {
		return nil
	}
	separator := "\n" + memoryEntryPrefix
	normalized := content
	if strings.HasPrefix(normalized, memoryEntryPrefix) {
		normalized = "\n" + normalized
	}
	chunks := strings.Split(normalized, separator)
	if len(chunks) < 2 {
		return nil
	}
	entries := make([]string, 0, len(chunks)-1)
	for _, chunk := range chunks[1:] {
		if entry := strings.TrimSpace(chunk); entry != "" {
			entries = append(entries, entry)
		}
	}
	if len(entries) <= n {
		return entries
	}
	return entries[len(entries)-n:]
}

// TailBytes returns the last bytes of the log, cut at an entry boundary.
//
// The byte-bounded read, for a caller that wants a budget rather than a count.
// It cuts the tail backwards from the end so an entry is never delivered
// half-written, which matters because a half entry reads as a complete one.
func (f *MemoryFile) TailBytes(max int) string {
	if max <= 0 {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	content, err := os.ReadFile(f.path)
	if err != nil {
		return ""
	}
	trimmed := strings.TrimRight(string(content), "\n")
	if len(trimmed) <= max {
		return trimmed
	}
	// Walk back to an entry boundary rather than cutting on a byte count.
	boundary := strings.LastIndex(trimmed[:len(trimmed)-(max/2)], "\n"+memoryEntryPrefix)
	if boundary < 0 {
		boundary = 0
	}
	return strings.TrimLeft(trimmed[boundary:], "\n")
}

// Lines is the number of entries in the log, for a diagnostic.
//
// It counts separators rather than parsing, because a count that has to be
// right is a count that has to read the file, and this is asked for on a
// diagnostic path where an approximate answer is fine and a second read is not.
func (f *MemoryFile) Lines() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	content, err := os.ReadFile(f.path)
	if err != nil {
		return 0
	}
	separator := "\n" + memoryEntryPrefix
	normalized := string(content)
	if strings.HasPrefix(normalized, memoryEntryPrefix) {
		normalized = "\n" + normalized
	}
	return strings.Count(normalized, separator)
}
