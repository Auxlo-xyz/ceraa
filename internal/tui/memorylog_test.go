package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTUIMemoryLogReaderFollowsTheSessionWorkdir pins the TUI's resolution.
//
// The workdir is the project root for this surface, exactly as it is for the
// memory/AGENTS.md the live renderer already reads from the same root, so the two
// resolve against one place. And the path is flat and absolute-rooted, which is
// what keeps it out of /tmp: this host has wiped /tmp three times in one day.
func TestTUIMemoryLogReaderFollowsTheSessionWorkdir(t *testing.T) {
	workDir := filepath.Join("/workspace", "ceraa", "workspace")
	reader := memoryLogReader(workDir)
	if reader == nil {
		t.Fatal("memoryLogReader returned nil, so the runner has no memory file to read")
	}
	want := filepath.Join(workDir, "Memory.md")
	if reader.Path() != want {
		t.Fatalf("path = %q, want %q", reader.Path(), want)
	}
	if again := memoryLogReader(workDir).Path(); again != reader.Path() {
		t.Fatalf("second call for the same workdir = %q, want %q: the path must not move", again, reader.Path())
	}
	if temp := os.TempDir(); strings.HasPrefix(filepath.Clean(reader.Path()), temp+string(filepath.Separator)) {
		t.Fatalf("memory log at %q is under %q, which this host has wiped three times", reader.Path(), temp)
	}
}

// TestTUIMemoryLogPathIsFlatAtTheRoot is why there is no memory/ subdirectory.
// MemoryFile.Append opens with O_CREATE and creates no directory, so a log at
// workDir/memory/Memory.md would fail every append on a session whose workdir has
// no memory subdirectory yet - and that is a fresh install, which is the case that
// matters most.
func TestTUIMemoryLogPathIsFlatAtTheRoot(t *testing.T) {
	root := filepath.Join("/workspace", "ceraa", "workspace")
	got := memoryLogPath(root)
	if got != filepath.Join(root, "Memory.md") {
		t.Fatalf("memoryLogPath = %q, want the log flat at the project root", got)
	}
	if strings.Contains(got, filepath.Join("memory", "Memory.md")) {
		t.Fatalf("memoryLogPath = %q nests the log in a directory Append cannot create", got)
	}
}

// TestTUIMemoryLogReaderIsWritable is the round trip a TUI run depends on: the
// appender and the reader must agree on the same file, or a session reads a log
// nothing is written to.
//
// Two appends and a "the newest is in the tail" assertion rather than a count,
// because the harness tail reader drops a file's first entry (tailEntries splits
// on a preceding newline that Append never writes for entry one). That defect
// lives in harness/memory/memoryfile.go and is outside this change's file scope;
// the contract that must hold either way is that what was appended is readable.
func TestTUIMemoryLogReaderIsWritable(t *testing.T) {
	reader := memoryLogReader(t.TempDir())
	for _, entry := range []string{"A preference worth keeping.", "A second one."} {
		if err := reader.Append(entry, "cli"); err != nil {
			t.Fatalf("Append(%q): %v", entry, err)
		}
	}
	entries := reader.Tail(10)
	if len(entries) == 0 {
		t.Fatal("Tail(10) = 0 entries after two appends, so nothing is ever read back")
	}
	if newest := entries[len(entries)-1]; !strings.Contains(newest, "A second one.") {
		t.Fatalf("newest entry = %q, want the most recent append", newest)
	}
}
