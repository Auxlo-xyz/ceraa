package coordinator

import (
	"sync"

	"ceraa/harness/instruction"
)

// instructionSource is what one run owns for instruction delivery: the compiled
// rules, the record of which of them have already been delivered, and the
// matcher that decides whether a rule's scope covers a path.
//
// # Why the snapshot is held rather than fetched
//
// Compiled once per run, never once per step. A file read per step is the
// exact defect this whole change exists to remove: project instructions are
// currently re-read into a prompt on every request, and moving delivery to the
// point of relevance is worth nothing if the load itself is per step. So the
// snapshot is resolved on first use and then held for the life of the run,
// which also makes a run's delivery a function of one fixed rule set rather
// than of however many times the file happened to change underneath it.
//
// Once per run, and not once per session, is the deliberate boundary. A rule
// file edited mid-run would otherwise change what the model is told partway
// through one piece of work, and the run would carry two revisions of the same
// project instruction with nothing marking which is current. The next run picks
// the edit up, which is the same boundary the sidekick's once-per-run standing
// set uses and for the same reason.
//
// # Why Seen is here and not derived
//
// instruction.Seen carries its own mutex, so it is safe to hand out directly
// and there is no reason to wrap it in another lock. It is per run rather than
// per session for the reason it is per run at all: a compaction drops the tool
// results that carried the notes, so the rules behind them are no longer in
// context and delivering them again is not a repeat. A set that outlived the
// transcript would retire a rule precisely when the context carrying it was
// rewritten. instruction.Reset is the seam an epoch takes.
//
// This is shaped like sidekickState deliberately - a runner-owned struct built
// once per run, holding mutable delivery state beside the lock that guards it.
// Two differently named holders doing one job is worse than one shape the next
// reader already knows how to look at.
type instructionSource struct {
	mu sync.Mutex
	// load compiles the project instructions. It is a function rather than a
	// store so that nothing here can refresh: the caller decides when rules are
	// recompiled, and holding only the result is what keeps that decision out of
	// the hot path.
	load func() instruction.Snapshot
	// snapshot and loaded are written under mu and read from it. loaded is
	// separate because a legitimately empty snapshot is not the same as no
	// snapshot yet, and a project with no AGENTS.md has to be able to stay
	// loaded.
	snapshot instruction.Snapshot
	loaded   bool
	// loads counts calls to load. It is the seam a test uses to see that the
	// snapshot was resolved once; without it "does not re-read" is a claim
	// about behaviour that a test can only check by reading a file itself.
	loads int
	// seen is the run's delivered set.
	seen *instruction.Seen
	// matcher decides scope. It is never nil after construction.
	matcher instruction.Matcher
}

// newInstructionSource returns a source for one run.
//
// A nil matcher is filled in with instructionMatcher rather than left nil. A nil
// Matcher is a meaningful value to instruction.Select - it withholds every
// scoped rule rather than guessing at one - but that is the answer to "no
// matcher is configured", and in this binary one is: memory has always been the
// package that knew how scope works. Defaulting to it means the ordinary
// construction cannot accidentally run with every scoped rule silently
// withheld, which is the failure that looks like a project with no rules.
func newInstructionSource(load func() instruction.Snapshot, matcher instruction.Matcher) *instructionSource {
	if matcher == nil {
		matcher = instructionMatcher
	}
	return &instructionSource{
		load:    load,
		seen:    instruction.NewSeen(),
		matcher: matcher,
	}
}

// Snapshot returns the run's compiled rules, loading them on first use.
//
// A nil source or a nil load yields an empty snapshot rather than a panic: the
// source is built on a run path that other agents own, and a run with no
// instruction store configured is a state, not a failure.
func (s *instructionSource) Snapshot() instruction.Snapshot {
	if s == nil {
		return instruction.Snapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return s.snapshot
	}
	// Marked loaded before the call rather than after, so load re-entering this
	// source cannot compile the same file twice.
	s.loaded = true
	s.loads++
	if s.load != nil {
		s.snapshot = s.load()
	}
	return s.snapshot
}

// Seen returns the run's delivered set, the same instance on every call.
//
// The identity matters as much as the contents: select.go exempts mandatory
// rules from the set, so the memory-side question of which rules are exempt is
// answered there, and all this has to guarantee is that one run shares one set.
func (s *instructionSource) Seen() *instruction.Seen {
	if s == nil {
		return instruction.NewSeen()
	}
	return s.seen
}

// Matcher returns the scope matcher, never nil.
func (s *instructionSource) Matcher() instruction.Matcher {
	if s == nil {
		return instructionMatcher
	}
	return s.matcher
}

// Loads reports how many times the rules were compiled for this run.
//
// Exported on the source rather than left as an unexported counter only because
// a runner wiring this up will want it in a run log: "instructions compiled
// once, 12 rules" is the observation that a per-step reload would take away.
func (s *instructionSource) Loads() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

// Select returns the rules that bear on one observed operation.
//
// The three holders combined: the snapshot loaded once, the run's delivered set,
// and the matcher. A convenience and nothing more - it passes the runner's
// defaults and returns what Select decided, including what it withheld.
//
// It deliberately does NOT mark anything delivered and does not render a note.
// That ordering is policy rather than plumbing: a rule has to be marked only
// after the note that carries it has survived its own byte ceiling, or a rule
// gets retired having never been seen. That belongs with whoever wires the
// runner, next to the sidekick's markDelivered, where the two delivery paths
// can be made to agree rather than each guessing.
func (s *instructionSource) Select(ev instruction.Trigger) instruction.Selection {
	if s == nil {
		return instruction.Selection{}
	}
	return instruction.Select(s.Snapshot(), ev, s.Seen(), instruction.Options{Matcher: s.Matcher()})
}
