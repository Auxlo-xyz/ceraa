package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// openSeededLog returns a MemoryFile over a log that already carries one line
// from before the separator existed.
//
// The seed is load-bearing rather than decoration. tailEntries drops everything
// before the first "\n## " as a preamble, and Append writes the first entry at
// offset zero with nothing before it, so on a log written from empty that first
// entry IS the preamble and never reaches Tail or Lines. A log that was ever
// migrated, hand-started or concatenated does not look like that, and it is the
// shape these tests want: it puts every appended entry behind a real separator
// so the invariants below are the ones being asserted. The drop is pinned on
// purpose, rather than left to be rediscovered, in
// TestTheFirstEntryOfALogIsDroppedByTailAndLines.
func openSeededLog(t *testing.T) *MemoryFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), "MEMORY.md")
	if err := os.WriteFile(path, []byte("# hand-written before the separator existed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return OpenMemoryFile(path)
}

// writeEntries builds a log with a fixed date, so every entry is the same known
// size. A fixture whose sizes are known is what lets a test prove that a byte
// cut would have landed inside an entry.
func writeEntries(t *testing.T, texts ...string) string {
	t.Helper()
	var b strings.Builder
	for _, text := range texts {
		b.WriteString(memoryEntryPrefix)
		b.WriteString("2026-01-02 03:04 s\n")
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	path := filepath.Join(t.TempDir(), "MEMORY.md")
	if err := os.WriteFile(path, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// mustAppend appends and fails the test on error, because every test below is
// about what was written and not about the write failing.
func mustAppend(t *testing.T, f *MemoryFile, text, session string) {
	t.Helper()
	if err := f.Append(text, session); err != nil {
		t.Fatalf("Append(%q, %q): %v", text, session, err)
	}
}

// dateLine is the part of an entry that is not the memory: the date the entry
// was learned, and the session when it has one.
func dateLine(entry string) string {
	line, _, _ := strings.Cut(entry, "\n")
	return line
}

// memoryOf is the entry with its date line removed, which is the text that was
// appended. It is what a reader is meant to receive.
func memoryOf(entry string) string {
	_, body, _ := strings.Cut(entry, "\n")
	return body
}

// endsOnABoundary reports whether s runs to the end of content and begins at an
// entry separator, which is the only way to know that no entry was cut in half.
func endsOnABoundary(content, s string) bool {
	if s == "" || !strings.HasSuffix(content, s) {
		return false
	}
	head := content[:len(content)-len(s)]
	return head == "" || strings.HasSuffix(head, "\n")
}

// excerpt keeps a failure readable: a memory can be thousands of bytes, and a
// test report that dumps one of them hides the line that reported it.
func excerpt(s string) string {
	const shown = 48
	if len(s) <= shown {
		return s
	}
	return s[:shown] + "..."
}

// The tail is what gets delivered into the prompt, so an entry has to come back
// as the text that was written rather than as a line of markup. The separator is
// the file's record structure and not the memory, and a reader handed it would
// see "## 2026-01-02 03:04" in front of its own note.
func TestAnAppendedEntryComesBackFromTailWithTheSeparatorStripped(t *testing.T) {
	f := openSeededLog(t)
	mustAppend(t, f, "the deploy target is the workers.dev host", "session-a")

	got := f.Tail(1)
	if len(got) != 1 {
		t.Fatalf("Tail(1) returned %d entries, want the one that was appended: %q", len(got), got)
	}
	if body := memoryOf(got[0]); body != "the deploy target is the workers.dev host" {
		t.Errorf("Tail returned %q, want the text that was appended", body)
	}
	if strings.Contains(got[0], memoryEntryPrefix) {
		t.Errorf("Tail handed back the separator with the entry: %q", got[0])
	}
	if line := dateLine(got[0]); len(line) < 16 {
		t.Errorf("the line above the memory is too short to hold a date: %q", line)
	} else if _, err := time.Parse("2006-01-02 15:04", line[:16]); err != nil {
		t.Errorf("the line above the memory is not a written date %q: %v", line, err)
	}
}

// The tail is read oldest-first and delivered newest-last, and a reversal would
// hand the agent its newest memory first and its oldest last: the same bytes
// carrying the opposite meaning. This ordering is the property the whole tail
// delivery rests on, so it is pinned rather than assumed.
func TestThreeAppendsComeBackOldestFirstAndNewestLast(t *testing.T) {
	f := openSeededLog(t)
	mustAppend(t, f, "the oldest thing learned", "session-a")
	mustAppend(t, f, "the middle thing learned", "session-b")
	mustAppend(t, f, "the newest thing learned", "session-c")

	got := f.Tail(3)
	if len(got) != 3 {
		t.Fatalf("Tail(3) returned %d entries, want all three that were appended", len(got))
	}
	for i, want := range []string{"the oldest thing learned", "the middle thing learned", "the newest thing learned"} {
		if body := memoryOf(got[i]); body != want {
			t.Errorf("entry %d reads %q, want %q", i, body, want)
		}
	}
	if body := memoryOf(got[len(got)-1]); body != "the newest thing learned" {
		t.Errorf("the last delivered entry is %q, want the newest", body)
	}
	// The same ordering one step narrower, because asking for as many entries as
	// there are takes a different path through the code than asking for fewer.
	if two := f.Tail(2); len(two) != 2 ||
		memoryOf(two[0]) != "the middle thing learned" ||
		memoryOf(two[1]) != "the newest thing learned" {
		t.Errorf("Tail(2) returned %q, want the middle then the newest", two)
	}
}

// Tail(n) is a count of entries and not a floor. A caller that asks for one
// memory and is handed three has asked for a budget and been given the file,
// which is the cost an append-only log exists to avoid.
func TestTailOfOneAfterThreeAppendsReturnsOnlyTheNewest(t *testing.T) {
	f := openSeededLog(t)
	mustAppend(t, f, "first", "session-a")
	mustAppend(t, f, "second", "session-b")
	mustAppend(t, f, "third", "session-c")

	got := f.Tail(1)
	if len(got) != 1 {
		t.Fatalf("Tail(1) returned %d entries, want exactly one", len(got))
	}
	if body := memoryOf(got[0]); body != "third" {
		t.Errorf("Tail(1) returned %q, want the third and last entry", body)
	}
	if two := f.Tail(2); len(two) != 2 || memoryOf(two[1]) != "third" {
		t.Errorf("Tail(2) returned %q, want the last two in order", two)
	}
}

// A first run has no log, and a caller that cannot read one must deliver
// nothing rather than fail a turn: the file is an optimisation and the
// conversation is the source of truth. Asking for nothing is the same question
// one step earlier, and neither is an error.
func TestTailOfZeroOfNothingOrOfAMissingFileIsEmptyNotAnError(t *testing.T) {
	f := OpenMemoryFile(filepath.Join(t.TempDir(), "absent", "MEMORY.md"))

	if got := f.Tail(1); len(got) != 0 {
		t.Errorf("Tail on a missing file returned %q, want nothing", got)
	}
	if got := f.Tail(0); len(got) != 0 {
		t.Errorf("Tail(0) returned %q, want nothing", got)
	}
	if got := f.Tail(-1); len(got) != 0 {
		t.Errorf("Tail(-1) returned %q, want nothing", got)
	}
	if got := f.TailBytes(50); got != "" {
		t.Errorf("TailBytes on a missing file returned %q, want nothing", got)
	}
	if got := f.TailBytes(0); got != "" {
		t.Errorf("TailBytes(0) returned %q, want nothing", got)
	}
	if got := f.TailBytes(-5); got != "" {
		t.Errorf("TailBytes(-5) returned %q, want nothing", got)
	}
	if n := f.Lines(); n != 0 {
		t.Errorf("Lines on a missing file returned %d, want 0", n)
	}
	if f.Path() == "" {
		t.Error("Path is empty, so a caller cannot tell a user where to look")
	}
}

// A blank entry is not a memory, and the file is the only record there is: a
// whitespace-only append would put a dated heading over nothing, and the next
// reader would count it as something the agent had learned.
func TestABlankAppendWritesNothingAtAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.md")
	f := OpenMemoryFile(path)
	for _, blank := range []string{"", "   ", "\n", "\t \n "} {
		if err := f.Append(blank, "session-a"); err != nil {
			t.Fatalf("Append(%q) errored: %v", blank, err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a blank append created the file: stat err %v", err)
	}

	log := openSeededLog(t)
	mustAppend(t, log, "the only real memory", "")
	before, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, blank := range []string{"", "   ", "\t\n"} {
		if err := log.Append(blank, "session-a"); err != nil {
			t.Fatalf("Append(%q) over a live log errored: %v", blank, err)
		}
	}
	after, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("a blank append grew the log from %d to %d bytes", len(before), len(after))
	}
	if n := log.Lines(); n != 1 {
		t.Errorf("blank appends left the count at %d, want the one real memory", n)
	}
	if !strings.Contains(string(after), "the only real memory") {
		t.Errorf("the real memory is not in the log:\n%s", after)
	}
}

// The session is how a memory gets scoped to one conversation, and it is
// written or omitted at append time because nothing later re-reads it. Losing
// the distinction would put one session's memory in front of another's reader.
func TestTheSessionTravelsOnTheLineThatCarriesTheDate(t *testing.T) {
	f := openSeededLog(t)
	mustAppend(t, f, "scoped to one conversation", "session-a")

	got := f.Tail(1)
	if len(got) != 1 {
		t.Fatalf("Tail(1) returned %d entries, want one", len(got))
	}
	if line := dateLine(got[0]); !strings.Contains(line, "session-a") {
		t.Errorf("the stored line is %q, which does not name the session", line)
	}

	mustAppend(t, f, "not scoped to anything", "")
	got = f.Tail(1)
	line := dateLine(got[0])
	if strings.Contains(line, "session") {
		t.Errorf("an entry appended without a session stored one anyway: %q", line)
	}
	if _, err := time.Parse("2006-01-02 15:04", line); err != nil {
		t.Errorf("the line without a session is not a bare date %q: %v", line, err)
	}
	if body := memoryOf(got[0]); body != "not scoped to anything" {
		t.Errorf("the unscoped entry reads %q", body)
	}
}

// The tail is bounded by entries and not by bytes, which is the whole reason a
// log can be append-only: a read that cost the size of everything ever learned
// would make the tail a full read and the log a store with extra steps. Three
// entries out of two hundred, each of them large, is that property.
func TestTailOnALargeLogAnswersByEntriesAndNotByBytes(t *testing.T) {
	f := openSeededLog(t)
	const total = 200
	bulk := strings.Repeat("learned detail ", 350)
	for i := 0; i < total; i++ {
		mustAppend(t, f, fmt.Sprintf("entry %03d %s", i, bulk), "session-a")
	}

	got := f.Tail(3)
	if len(got) != 3 {
		t.Fatalf("Tail(3) on a %d-entry log returned %d entries, want exactly 3", total, len(got))
	}
	for i, want := range []string{"entry 197 ", "entry 198 ", "entry 199 "} {
		if body := memoryOf(got[i]); !strings.HasPrefix(body, want) {
			t.Errorf("delivered entry %d starts %q, want %q", i, excerpt(body), want)
		}
	}
	if n := f.Lines(); n != total {
		t.Errorf("Lines reports %d on a %d-entry log", n, total)
	}
	if all := f.Tail(total); len(all) != total {
		t.Errorf("Tail(%d) returned %d, so the read did not reach the oldest entry", total, len(all))
	}
}

// A half entry reads as a complete one, so a byte-bounded read that lands in the
// middle of a memory delivers a truncated note as though it were whole. The
// entries here are a known size so the cut provably falls inside one.
func TestTailBytesCutsOnAnEntryBoundaryAndNeverInsideAnEntry(t *testing.T) {
	const body = "abcdefghij0123456789"
	const header = memoryEntryPrefix + "2026-01-02 03:04 s\n"
	const stride = len(header) + len(body) + 2
	path := writeEntries(t, body, body, body, body)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimRight(string(raw), "\n")
	lastTextStart := 3*stride + len(header)
	if naive := len(trimmed) - 15; naive <= lastTextStart {
		t.Fatalf("the fixture is not adversarial: a 15-byte cut lands at %d, at or before the last entry's text at %d", naive, lastTextStart)
	}

	f := OpenMemoryFile(path)
	for _, max := range []int{1, 15, 20, stride - 1, stride, stride + 1, 2*stride - 1, 2 * stride} {
		got := f.TailBytes(max)
		if got == "" {
			t.Errorf("TailBytes(%d) returned nothing on a log that has entries", max)
			continue
		}
		if !strings.HasPrefix(got, memoryEntryPrefix) {
			t.Errorf("TailBytes(%d) starts inside an entry: %q", max, got)
		}
		if !endsOnABoundary(trimmed, got) {
			t.Errorf("TailBytes(%d) is not a run of whole entries ending at the end of the log: %q", max, got)
		}
	}
}

// The budget yields to whole-entry integrity and not the other way round: an
// entry larger than the request comes back whole, because a truncated memory is
// indistinguishable from a short one.
func TestAnEntryLargerThanTheBudgetComesBackWhole(t *testing.T) {
	oversized := strings.Repeat("q", 500)
	path := writeEntries(t, "small", oversized)
	f := OpenMemoryFile(path)

	got := f.TailBytes(10)
	if !strings.HasPrefix(got, memoryEntryPrefix) || !strings.HasSuffix(got, oversized) {
		t.Errorf("TailBytes(10) did not return the oversized entry whole: %d bytes", len(got))
	}
	if len(got) <= 10 {
		t.Errorf("TailBytes(10) cut the entry to %d bytes instead of keeping it whole", len(got))
	}
}

// The byte budget only bites once the log outgrows it, so asking for more than
// there is must hand back everything rather than truncate. The trailing blank
// line is spacing between entries and not content of its own.
func TestTailBytesUnderTheBudgetReturnsTheWholeLogTrimmed(t *testing.T) {
	path := writeEntries(t, "one", "two", "three")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimRight(string(raw), "\n")
	f := OpenMemoryFile(path)

	for _, max := range []int{len(trimmed), len(trimmed) + 1, 10 * len(trimmed)} {
		if got := f.TailBytes(max); got != trimmed {
			t.Errorf("TailBytes(%d) returned %d bytes, want the whole %d-byte log", max, len(got), len(trimmed))
		}
	}
	if got := f.TailBytes(1000); strings.HasSuffix(got, "\n") {
		t.Error("TailBytes kept the trailing blank line")
	}
	// Walking back to a boundary rather than cutting on a byte count means the
	// delivered tail is never gratuitously small: on a log that outgrows the
	// budget, at least half of it is delivered. Under the budget the whole log
	// is, which the loop above already holds.
	for max := 10; max < len(trimmed); max += 7 {
		if got := f.TailBytes(max); len(got) < max/2 {
			t.Errorf("TailBytes(%d) returned %d bytes, under half the budget", max, len(got))
		}
	}
}

// Lines is what a user is told when the log looks wrong, so it has to agree with
// what Tail delivers: a count that disagrees with the tail sends the reader
// looking in the wrong place.
func TestLinesCountsEveryEntryWritten(t *testing.T) {
	f := openSeededLog(t)
	if n := f.Lines(); n != 0 {
		t.Errorf("a log with nothing appended reports %d entries", n)
	}
	for want := 1; want <= 10; want++ {
		mustAppend(t, f, fmt.Sprintf("memory %d", want), "session-a")
		if n := f.Lines(); n != want {
			t.Fatalf("after %d appends Lines reports %d", want, n)
		}
	}
	if delivered := len(f.Tail(f.Lines())); delivered != f.Lines() {
		t.Errorf("Tail(Lines()) delivered %d entries against a count of %d", delivered, f.Lines())
	}
}

// A whole document delivered as a single memory is worse than none, so a log
// written before this format existed has to parse as no entries at all rather
// than as one giant one.
func TestALogFromBeforeTheSeparatorParsesAsNoEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.md")
	legacy := "Deploy notes from the old memory file.\nTwo lines of them, and no separator anywhere.\n"
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	f := OpenMemoryFile(path)

	if got := f.Tail(5); len(got) != 0 {
		t.Errorf("a log with no separators parsed as %d entries: %q", len(got), got)
	}
	if n := f.Lines(); n != 0 {
		t.Errorf("a log with no separators reports %d entries", n)
	}
	if got := f.TailBytes(1000); got != strings.TrimRight(legacy, "\n") {
		t.Errorf("TailBytes is a byte read and should have handed the file back: %q", got)
	}
}

// The two-line format exists so a value containing a newline survives, because a
// single flat line cannot without an escaping rule the next reader has to know.
// If an entry split, one memory would be delivered as two half ones.
func TestAnEntryWithANewlineInsideItStaysOneEntry(t *testing.T) {
	f := openSeededLog(t)
	mustAppend(t, f, "a memory that was one line", "session-a")
	mustAppend(t, f, "first line of it\nsecond line of it\nthird line of it", "session-b")
	mustAppend(t, f, "and then a third memory", "session-c")

	got := f.Tail(3)
	if len(got) != 3 {
		t.Fatalf("Tail(3) returned %d entries, want three: an entry holding a newline split or vanished", len(got))
	}
	want := "first line of it\nsecond line of it\nthird line of it"
	if body := memoryOf(got[1]); body != want {
		t.Errorf("the multi-line entry came back as %q, want it whole as %q", body, want)
	}
	if n := strings.Count(got[1], "\n"); n != 3 {
		t.Errorf("the multi-line entry carried %d line breaks, want one per written line", n)
	}
	if body := memoryOf(f.Tail(1)[0]); body != "and then a third memory" {
		t.Errorf("Tail(1) after a multi-line entry returned %q", body)
	}
	if n := f.Lines(); n != 3 {
		t.Errorf("Lines reports %d where three entries were written", n)
	}
}

// The log is the one file the agent cannot corrupt by rewriting it, which only
// holds while simultaneous appends interleave cleanly: a lost entry is a memory
// the agent believes it wrote.
func TestConcurrentAppendsLoseNothing(t *testing.T) {
	f := openSeededLog(t)
	const writers = 10
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := f.Append(fmt.Sprintf("memory from writer %02d", i), "session-a"); err != nil {
				t.Errorf("writer %02d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	raw, err := os.ReadFile(f.Path())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < writers; i++ {
		want := fmt.Sprintf("memory from writer %02d", i)
		if n := strings.Count(string(raw), want); n != 1 {
			t.Errorf("%q appears %d times in the log, want once", want, n)
		}
	}
	if n := f.Lines(); n != writers {
		t.Errorf("Lines reports %d after %d concurrent appends", n, writers)
	}
	got := f.Tail(writers)
	if len(got) != writers {
		t.Fatalf("Tail(%d) returned %d entries", writers, len(got))
	}
	for i := 0; i < writers; i++ {
		want := fmt.Sprintf("writer %02d", i)
		delivered := false
		for _, entry := range got {
			if strings.Contains(entry, want) {
				delivered = true
			}
		}
		if !delivered {
			t.Errorf("no delivered entry carries %q", want)
		}
	}
}

// The file is the source of truth and not the handle: a second handle opened on
// the same path has to see what the first wrote, or a memory would depend on
// which object happened to be holding it.
func TestTwoHandlesOnOnePathAreTwoViewsOfTheSameLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.md")
	if err := os.WriteFile(path, []byte("# hand-written before the separator existed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first := OpenMemoryFile(path)
	second := OpenMemoryFile(path)
	if first.Path() != path || second.Path() != path {
		t.Fatalf("the handles do not name the path they were opened on: %q and %q", first.Path(), second.Path())
	}

	mustAppend(t, first, "written through the first handle", "session-a")
	mustAppend(t, second, "written through the second handle", "session-b")
	mustAppend(t, first, "written through the first handle again", "session-c")

	handles := []*MemoryFile{first, second, OpenMemoryFile(path)}
	for _, handle := range handles {
		got := handle.Tail(3)
		if len(got) != 3 {
			t.Fatalf("a handle sees %d entries, want all three", len(got))
		}
		if body := memoryOf(got[0]); body != "written through the first handle" {
			t.Errorf("the oldest entry reads %q across handles", body)
		}
		if body := memoryOf(got[1]); body != "written through the second handle" {
			t.Errorf("the middle entry reads %q across handles", body)
		}
		if body := memoryOf(got[2]); body != "written through the first handle again" {
			t.Errorf("the newest entry reads %q across handles", body)
		}
		if n := handle.Lines(); n != 3 {
			t.Errorf("a handle counts %d entries across the same log", n)
		}
	}
}

// FINDING, pinned rather than hidden.
//
// tailEntries drops everything before the first "\n## " as a preamble, but
// The first entry of a log written from empty has no newline before its
// separator, so a split that discards the piece before the first separator
// treats it as the file's preamble and drops it. Measured before the fix: one
// append read back as zero, five as four, and the first thing a project ever
// learned was permanently invisible with nothing reporting it.
//
// This test was written as the finding and is the one to turn green when the drop
// is fixed, which is why its comment says so rather than describing a passing
// state.
func TestTheFirstEntryOfALogIsReadBackLikeEveryOther(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Memory.md")
	log := OpenMemoryFile(path)
	if err := log.Append("the only memory in the log", "session-a"); err != nil {
		t.Fatal(err)
	}

	got := log.Tail(50)
	if len(got) != 1 {
		t.Fatalf("Tail returned %d entries for a one-entry log, want exactly 1: %#v", len(got), got)
	}
	if !strings.Contains(got[0], "the only memory in the log") {
		t.Errorf("the first entry is not readable: %q", got[0])
	}
	if lines := log.Lines(); lines != 1 {
		t.Errorf("Lines() = %d for a one-entry log, want 1", lines)
	}
}
