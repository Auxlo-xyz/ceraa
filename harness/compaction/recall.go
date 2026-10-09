package compaction

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"ceraa/harness/sessionstore"
)

// recallHits caps one search: a pattern that matches the whole log is a denial
// of detail, not information, and the way back is narrowing the pattern.
const recallHits = 20

// recallOpenChars caps one opened entry. It matches the runner's tool result
// cap, so an opened head neither re-spools nor re-trims on its way back.
const recallOpenChars = 12000

// ReadArchive loads a session's retained compactions, oldest first. It reports
// whether the sidecar exists at all, so a caller can tell "nothing was ever
// archived" from "the archive failed to decode".
func ReadArchive(dir, sessionID string) (ArchiveFile, bool, error) {
	var archive ArchiveFile
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(sessionID) == "" {
		return archive, false, nil
	}
	raw, err := os.ReadFile(archivePath(dir, sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return archive, false, nil
		}
		return archive, false, err
	}
	if err := json.Unmarshal(raw, &archive); err != nil {
		return archive, false, err
	}
	return archive, true, nil
}

// SearchArchive finds pattern in a session's retained compactions: summaries
// and discarded messages, newest entry first. Each hit names its entry number
// for OpenArchive and carries the matching lines with their roles. Entries and
// hits are both capped.
//
// The archive was written redacted, so reading it is no more exposing than
// reading the session it summarises. What it is not is current: an entry is a
// photograph of a conversation as it was, and the repo may have moved since.
func SearchArchive(dir, sessionID, pattern string) (string, error) {
	archive, exists, err := ReadArchive(dir, sessionID)
	if err != nil {
		return "", err
	}
	if !exists || len(archive.Entries) == 0 {
		return "No compactions archived for this session.", nil
	}
	expr, err := regexp.Compile("(?i:" + pattern + ")")
	if err != nil {
		return "", fmt.Errorf("bad pattern %q: %v", pattern, err)
	}
	var b strings.Builder
	hits := 0
	full := false
	for e := len(archive.Entries) - 1; e >= 0 && !full; e-- {
		entry := archive.Entries[e]
		for _, line := range strings.Split(entry.Summary, "\n") {
			if !expr.MatchString(line) {
				continue
			}
			fmt.Fprintf(&b, "entry %d (summary, seq %d, %s): %s\n", e, entry.Seq, entry.At.Format(time.DateOnly), truncateRecallLine(line))
			hits++
			if hits >= recallHits {
				full = true
				break
			}
		}
		if full {
			break
		}
		for _, m := range entry.Discarded {
			for _, line := range recallMessageLines(m) {
				if !expr.MatchString(line) {
					continue
				}
				fmt.Fprintf(&b, "entry %d (seq %d, %s): %s\n", e, entry.Seq, entry.At.Format(time.DateOnly), truncateRecallLine(line))
				hits++
				if hits >= recallHits {
					full = true
					break
				}
			}
			if full {
				break
			}
		}
	}
	if hits == 0 {
		return "No match in the archived history.", nil
	}
	if full {
		b.WriteString("Newest 20 hits shown; narrow the pattern for the rest.")
	}
	return strings.TrimSpace(b.String()), nil
}

// OpenArchive renders one retained entry whole: its summary, then its
// discarded messages oldest first, capped. Entry numbers count from zero at
// the oldest retained, and search results name the number to open. An entry
// whose head was dropped for size says so instead of showing a partial head
// that looks whole.
func OpenArchive(dir, sessionID string, entry int) (string, error) {
	archive, exists, err := ReadArchive(dir, sessionID)
	if err != nil {
		return "", err
	}
	if !exists || len(archive.Entries) == 0 {
		return "No compactions archived for this session.", nil
	}
	if entry < 0 || entry >= len(archive.Entries) {
		return "", fmt.Errorf("no entry %d: the archive holds %d", entry, len(archive.Entries))
	}
	e := archive.Entries[entry]
	var b strings.Builder
	fmt.Fprintf(&b, "entry %d (seq %d, %s):\n%s\n", entry, e.Seq, e.At.Format(time.DateOnly), e.Summary)
	if e.Truncated {
		b.WriteString("[the discarded head was dropped for size; the summary above is the whole record]")
		return b.String(), nil
	}
	for _, m := range e.Discarded {
		for _, line := range recallMessageLines(m) {
			b.WriteString(line + "\n")
			if b.Len() >= recallOpenChars {
				b.WriteString("[capped: narrow with search instead of opening whole]")
				return strings.TrimSpace(b.String()), nil
			}
		}
	}
	return strings.TrimSpace(b.String()), nil
}

// recallMessageLines renders one archived message as searchable lines. Roles
// travel with their lines, because a match without its speaker is a quote
// without attribution, and tool results travel whole: matching happens here,
// and the cap is applied by the caller that prints.
func recallMessageLines(m sessionstore.Message) []string {
	role := string(m.Role)
	if role == "" {
		role = "message"
	}
	var lines []string
	if strings.TrimSpace(m.Content) != "" {
		for _, line := range strings.Split(m.Content, "\n") {
			if strings.TrimSpace(line) != "" {
				lines = append(lines, "["+role+"] "+line)
			}
		}
	}
	for _, call := range m.AllToolCalls() {
		if call.Name != "" {
			lines = append(lines, "["+role+" tool call] "+call.Name+"("+call.Args+")")
		}
	}
	if strings.TrimSpace(m.ToolResult) != "" {
		for _, line := range strings.Split(m.ToolResult, "\n") {
			if strings.TrimSpace(line) != "" {
				lines = append(lines, "[tool result] "+line)
			}
		}
	}
	return lines
}

func truncateRecallLine(line string) string {
	const max = 300
	trimmed := strings.TrimSpace(line)
	if len(trimmed) <= max {
		return trimmed
	}
	if i := strings.LastIndex(trimmed[:max], " "); i > 0 {
		return trimmed[:i] + "..."
	}
	return trimmed[:max] + "..."
}
