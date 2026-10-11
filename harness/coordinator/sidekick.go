package coordinator

import (
	"ceraa/harness/memory"
)

// defaultMemoryTail is how many entries of the append-only log the tail reader
// takes when the caller has not said.
//
// Derived rather than picked. It was sized for per-result notes (general plus
// targeted limits times the notes a run could write); the once-per-run tail
// line caps itself in runes on top, so a large tail costs nothing the line does
// not render.
const defaultMemoryTail = (memory.SidekickGeneralLimit + memory.SidekickTargetLimit) *
	(memory.SidekickRunBytes / memory.SidekickBlockBytes)

// sidekickState is the reader for the run's memory: the append-only log the
// once-per-run tail line renders from.
//
// It used to annotate every tool result and carry a delivered set, a run
// ceiling and seeding entry points to support that. Per-result delivery is
// retired: memory reaches the model exactly once per run as a suffix on the
// newest user message (Runner.memoryTailLine), so there is nothing to dedup,
// nothing to ceiling, and nothing to seed. What stays is the reader, because
// the file on disk is the source of truth and a snapshot taken at construction
// diverges from it silently in a long-lived daemon.
type sidekickState struct {
	// memFile is the append-only log this run reads what has been learned from.
	memFile func() *memory.MemoryFile
	// tail is how many entries memFile is read for. Zero never reaches here;
	// useMemoryFile substitutes defaultMemoryTail.
	tail int
	// workDir scopes the target extractor a sibling file shares.
	workDir string
}

func newSidekickState(workDir string) *sidekickState {
	return &sidekickState{workDir: workDir}
}

// useMemoryFile makes the append-only log this state's source of what has been
// learned.
func (s *sidekickState) useMemoryFile(file func() *memory.MemoryFile, tail int) {
	if s == nil || file == nil {
		return
	}
	if tail <= 0 {
		tail = defaultMemoryTail
	}
	s.memFile, s.tail = file, tail
}

// memTail is the newest entries of the log, oldest first, newest last, or nil
// when there is nothing to read.
//
// An unreadable file is an empty tail rather than an error: the conversation is
// the source of truth and the log is an optimisation, so a run that cannot read
// it delivers nothing instead of failing a turn.
func (s *sidekickState) memTail() []string {
	if s == nil || s.memFile == nil {
		return nil
	}
	file := s.memFile()
	if file == nil {
		return nil
	}
	return file.Tail(s.tail)
}
