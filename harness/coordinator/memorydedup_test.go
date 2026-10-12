package coordinator

import (
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/memory"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// Session.MemoryDelivered was documented as the set of memories already told to
// the model, and nothing wrote it. The tail therefore re-rendered into every
// request forever, which is the behaviour these pin shut.

func dedupRunner(t *testing.T, path string) *Runner {
	t.Helper()
	return New(RunnerConfig{
		MemoryDelivery: string(memory.DeliverySidekick),
		MemoryFile:     logMemoryFile(path),
	}, nil, tool.NewRegistry())
}

// The core of the fix: what has been delivered is not delivered again.
func TestDeliveredMemoryIsNotSentOnTheNextTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Memory.md")
	appendLog(t, memory.OpenMemoryFile(path), "DEDUP: /srv/api is the backend")
	runner := dedupRunner(t, path)
	sess := &sessionstore.Session{}

	tail := runner.memoryTailLine(sess)
	if !strings.Contains(tail, "DEDUP") {
		t.Fatalf("the first turn delivered nothing: %q", tail)
	}
	// What the run records as having rode is what is marked delivered.
	sess.MarkDeliveredMemory(runner.pendingMemory)

	if again := runner.memoryTailLine(sess); again != "" {
		t.Errorf("memory was delivered a second time: %q", again)
	}
}

// Delivery is per entry, not per session. A later entry still has to arrive, or
// the fix would silence memory permanently after the first note.
func TestANewEntryRidesAfterTheEarlierOneWasDelivered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Memory.md")
	file := memory.OpenMemoryFile(path)
	appendLog(t, file, "FIRST: /srv/api is the backend")
	runner := dedupRunner(t, path)
	sess := &sessionstore.Session{}

	runner.memoryTailLine(sess)
	sess.MarkDeliveredMemory(runner.pendingMemory)
	appendLog(t, file, "SECOND: run gofmt before committing")

	tail := runner.memoryTailLine(sess)
	if !strings.Contains(tail, "SECOND") {
		t.Errorf("the new entry did not ride: %q", tail)
	}
	if strings.Contains(tail, "FIRST") {
		t.Errorf("the delivered entry rode again alongside the new one: %q", tail)
	}
}

// A rebuild that finds nothing must not clear what the real request carried.
// The compactor and the preview both rebuild messages, and a clear here would
// strand every id in the set as permanently undelivered.
func TestARebuildThatFindsNothingLeavesTheDeliveredSetIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Memory.md")
	appendLog(t, memory.OpenMemoryFile(path), "KEPT: the tail only ever grows")
	runner := dedupRunner(t, path)
	sess := &sessionstore.Session{}

	runner.memoryTailLine(sess)
	carried := runner.pendingMemory
	sess.MarkDeliveredMemory(carried)

	if tail := runner.memoryTailLine(sess); tail != "" {
		t.Fatalf("the rebuild found something to send: %q", tail)
	}
	// Retained, not cleared. Run-record marking reads pendingMemory, so a rebuild
	// that cleared it here would leave the ids that really rode unmarked and send
	// them again on every following turn.
	if len(runner.pendingMemory) != len(carried) {
		t.Errorf("a rebuild with nothing new changed the carried ids: %v, want %v",
			runner.pendingMemory, carried)
	}
	// And so the delivered set is exactly the ids the model was sent.
	if len(sess.MemoryDelivered) != len(carried) {
		t.Errorf("delivered = %v, want the %d ids that rode", sess.MemoryDelivered, len(carried))
	}
}

// An entry cut off by the budget was not delivered, so it must not be marked.
// Truncating first and recording everything would lose the remainder for good.
func TestAnEntryCutByTheBudgetIsNotMarkedDelivered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Memory.md")
	appendLog(t, memory.OpenMemoryFile(path),
		strings.Repeat("a ", 300),
		strings.Repeat("b ", 300),
	)
	runner := dedupRunner(t, path)
	sess := &sessionstore.Session{}

	tail := runner.memoryTailLine(sess)
	if !strings.Contains(tail, "a ") {
		t.Fatalf("the first entry did not ride: %q", tail)
	}
	if len(runner.pendingMemory) != 1 {
		t.Fatalf("recorded %d ids, want only the entry that was sent: %v",
			len(runner.pendingMemory), runner.pendingMemory)
	}
	// So the entry that did not fit is still eligible for a later turn.
	sess.MarkDeliveredMemory(runner.pendingMemory)
	if next := runner.memoryTailLine(sess); !strings.Contains(next, "b ") {
		t.Errorf("the entry that did not fit was lost rather than carried over: %q", next)
	}
}

// Two identical entries are one memory. Hashing position or date would let the
// second reuse the first's id and be silently dropped.
func TestEntryIDIsStableForTheSameTextAndDiffersOtherwise(t *testing.T) {
	a := memory.EntryID("the tail only ever grows")
	b := memory.EntryID("  the tail only ever grows  ")
	c := memory.EntryID("something else entirely")
	if a != b {
		t.Errorf("the same memory hashed two ways: %q and %q", a, b)
	}
	if a == c {
		t.Errorf("two different memories share the id %q", a)
	}
	if !strings.HasPrefix(a, "memfile:") {
		t.Errorf("id %q does not carry the source prefix", a)
	}
}

// Marking is idempotent, because the same set is written by the run record and
// a fold carries it forward. A duplicate would grow the file every turn.
func TestMarkingTheSameMemoryTwiceIsOneEntry(t *testing.T) {
	sess := &sessionstore.Session{}
	sess.MarkDeliveredMemory([]string{"memfile:aaaa", "memfile:bbbb"})
	sess.MarkDeliveredMemory([]string{"memfile:aaaa"})
	if len(sess.MemoryDelivered) != 2 {
		t.Errorf("delivered = %v, want two entries", sess.MemoryDelivered)
	}
}
