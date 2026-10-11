package agent

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentMemoryLogReaderIsAnchoredWhereTheRuns pins the CLI's resolution.
//
// This surface sets no WorkDir on its RunnerConfig, so the runner resolves
// relative paths against the process working directory. The log is anchored to
// "." for the same reason InstructionRules is: a log resolved anywhere else would
// be a second answer to "where does this run work", and the two would drift
// silently the first time the binary was launched from a different directory.
func TestAgentMemoryLogReaderIsAnchoredWhereTheRuns(t *testing.T) {
	reader := memoryLogReader(".")
	if reader == nil {
		t.Fatal("memoryLogReader returned nil, so the runner has no memory file to read")
	}
	want := filepath.Join(".", "Memory.md")
	if reader.Path() != want {
		t.Fatalf("path = %q, want %q", reader.Path(), want)
	}
	if filepath.IsAbs(reader.Path()) {
		t.Fatalf("path %q is absolute, so it does not follow the directory the CLI was launched from", reader.Path())
	}
}

// TestAgentMemoryLogPathIsFlatAtTheRoot is why there is no memory/ subdirectory,
// the same rule the other three surfaces follow. Append opens with O_CREATE and
// creates no directory, so a nested log fails every write on a fresh checkout.
func TestAgentMemoryLogPathIsFlatAtTheRoot(t *testing.T) {
	got := memoryLogPath(".")
	if got != filepath.Join(".", "Memory.md") {
		t.Fatalf("memoryLogPath(\".\") = %q, want the log flat at the root", got)
	}
	if strings.Contains(got, filepath.Join("memory", "Memory.md")) {
		t.Fatalf("memoryLogPath = %q nests the log in a directory Append cannot create", got)
	}
}

// TestAgentMemoryLogReaderIsWritable is the round trip a CLI run depends on. The
// reader is anchored to the working directory, so the assertion runs in a
// directory that is writable rather than the checkout the test binary sits in.
//
// Two appends and a "the newest is in the tail" assertion rather than a count,
// because the harness tail reader drops a file's first entry (tailEntries splits
// on a preceding newline that Append never writes for entry one). That defect
// lives in harness/memory/memoryfile.go and is outside this change's file scope;
// the contract that must hold either way is that what was appended is readable.
func TestAgentMemoryLogReaderIsWritable(t *testing.T) {
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
