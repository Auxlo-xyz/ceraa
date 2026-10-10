package coordinator

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/internal/logger"
)

// sidekickBondBytes bounds the bond block appended to a tool result.
//
// It repeats the budget the renderer is configured with at every call site, and
// it is here anyway because a renderer arrives from a caller: bounding at the
// point the text becomes output is the only place a bound cannot be walked past,
// which is the reason oneLine bounds rather than briefOf.
//
// Over the limit is refused rather than truncated. The block is a wrapper around
// an operator-maintained file whose own renderer already truncates while keeping
// the read-only law, so cutting it a second time is the one edit that can drop
// that law - and a half-open <bond> reads to the model as a damaged file rather
// than as a misconfigured budget.
const sidekickBondBytes = 2000

// sidekickDeliveredKeep bounds the delivered set carried on a session file.
//
// Every entry is a 32-character id, so the list is small beside the transcript it
// rides in, but nothing else on a session shrinks and a session outlives any
// single store. The oldest ids are the ones forgotten: forgetting one costs a
// single repeated delivery much later, which is the recoverable direction.
const sidekickDeliveredKeep = 512

// sidekickState carries what tool-output delivery needs, and records what it
// delivered so the two modes can be told apart later.
//
// The store is read through a function rather than held, for the same reason the
// prompt block is: the file on disk is the source of truth, and a snapshot taken
// at construction diverges from it silently in a long-lived daemon.
type sidekickState struct {
	mu     sync.Mutex
	lookup func() ([]memory.Entry, error)
	// bond renders the operator's identity file. It is a separate channel from
	// lookup because it is not a memory: it has no `applies`, no score and no
	// gate, and routing it through the store is what dropped it from the prompt
	// in the first place.
	bond    func() (string, error)
	workDir string

	// delivered is every memory already annotated into this session's transcript,
	// keyed by memory.DeliveryID.
	//
	// It exists because a targeted memory is fired by its own trigger, over and
	// over: a memory scoped to /srv/api reaches every read and every write of a
	// file beneath it, and each copy is written into the tool result and re-sent
	// on every later request. The general set had one bool guarding exactly this,
	// and the bool was never extended to the targeted half - which is the half
	// that repeats on a trigger of its own rather than on every call.
	delivered map[string]struct{}

	// generalDelivered makes the standing preferences fire once per run rather than
	// once per tool call.
	//
	// A general memory has no path, so it matches every operation. Injecting it
	// each time would write it into the transcript once per call - forty copies in
	// a long run, against a prompt block that carried it once - and the copies are
	// not free: every one sits in the context for the rest of the session.
	//
	// Kept rather than folded into the delivered set above, and the reason is scope
	// rather than convenience. A standing preference has no trigger to fire on, so
	// the only question about it is how often to repeat it, and once per run is
	// the affordable answer: three lines once per run against three lines once per
	// call. Once per session would fail in the other direction - the baseline is
	// the thing that has to survive a compacted transcript, and a set that never
	// releases it stops delivering it precisely when the context carrying it was
	// rewritten.
	generalDelivered bool

	// bondDelivered makes the bond fire once per run, for the same reason. A bond
	// has no path at all, so it matches every operation even harder than a general
	// memory does, and it is a longer document: the whole of BOND.md, once per
	// tool call, is the flooding this field exists to prevent.
	bondDelivered bool

	// runSpent is the bytes of memory note this run has already written into tool
	// results. Per run rather than per session because the ceiling is about
	// attention inside one piece of work, and a session is not one piece of work.
	//
	// The bond is not counted against it. The bond is a different channel with its
	// own budget above and its own once-per-run rule, and charging it to the memory
	// ceiling would silently disable memory for any run whose operator file is
	// large.
	runSpent int

	report memory.DeliveryReport
}

func newSidekickState(lookup func() ([]memory.Entry, error), bond func() (string, error), workDir string) *sidekickState {
	return &sidekickState{
		lookup:    lookup,
		bond:      bond,
		workDir:   workDir,
		delivered: map[string]struct{}{},
	}
}

// annotate returns the sidekick block for one completed operation, or "" when
// nothing qualifies.
func (s *sidekickState) annotate(sess *sessionstore.Session, result operation.ResultEvent) string {
	// The bond goes first and ahead of the store read below.
	//
	// Ahead, because it must not sit behind a read that can fail: an unreadable
	// project-memory store would then take the person's name and standing rules
	// down with it, and that is the same silent loss this channel exists to stop.
	// A missing BOND.md renders empty and that is a first run, not a failure.
	//
	// First, because memory.Combine puts the bond ahead of project memory in the
	// prompt, and identity reading before project context is the order a reader
	// expects in both places.
	bond := s.bondBlock()

	if s == nil || s.lookup == nil {
		return bond
	}
	entries, err := s.lookup()
	if err != nil {
		// A read failure must not fail the turn, and must not look like "there is
		// no memory", which would be a different and wrong answer.
		return bond
	}
	now := time.Now()
	targets := s.targetsFor(sess, result)

	s.mu.Lock()
	defer s.mu.Unlock()
	// Folded in before the decision rather than seeded at construction: the runner
	// is built per run, and the session file is the only thing that outlives one.
	s.adoptDelivered(sess)

	var targeted, general []memory.Brief
	if len(targets) > 0 {
		targeted = memory.Select(entries, targets, now, memory.SidekickTargetLimit, s.alreadyDelivered)
	}
	if !s.generalDelivered {
		general = memory.SelectGeneral(entries, now, memory.SidekickGeneralLimit)
	}
	if len(targeted) == 0 && len(general) == 0 {
		s.report.NoteSuppressed(belowGate(entries, now))
		s.report.Deduped += s.wouldDeliver(entries, targets, now)
		return bond
	}
	note := memory.Annotate(targets, targeted, general)
	if note == "" {
		return bond
	}
	// The run ceiling is checked against the rendered note rather than against what
	// has been spent so far, so it is a ceiling and not a threshold the last block
	// steps over. The only safe thing to drop here is the whole note: half a block
	// costs the same bytes as a whole one and reads as if the list were complete.
	//
	// The general set is deliberately left unarmed when this fires, so a preference
	// that missed its one chance still gets it on a later call rather than being
	// lost for the run.
	if s.runSpent+len(note) > memory.SidekickRunBytes {
		s.report.BudgetWithheld += len(targeted) + len(general)
		return bond
	}
	// Only what was actually rendered is marked delivered. Marking a selection the
	// block ceiling threw away would retire a memory the model never saw.
	s.markDelivered(sess, targeted)
	s.generalDelivered = true
	s.runSpent += len(note)
	// Memories delivered rather than operations offered: after dedup an operation
	// that matched nothing is not an injection, and counting it as one would make a
	// run where dedup did all the work look like the run this replaced.
	s.report.TargetedInjects += len(targeted)
	s.report.GeneralInjected = len(general)
	return bond + note
}

// alreadyDelivered is the exclusion predicate handed to memory.Select.
func (s *sidekickState) alreadyDelivered(id string) bool {
	_, ok := s.delivered[id]
	return ok
}

// wouldDeliver is how many memories this operation would have been annotated with
// had nothing been delivered yet.
//
// Recorded so a session where dedup suppressed everything still reads as activity
// rather than as a store with nothing to say, which is the difference between "the
// store was empty" and "the store was already in context".
func (s *sidekickState) wouldDeliver(entries []memory.Entry, targets []string, now time.Time) int {
	if len(targets) == 0 || len(s.delivered) == 0 {
		return 0
	}
	return len(memory.Select(entries, targets, now, memory.SidekickTargetLimit, nil))
}

// adoptDelivered folds the session's persisted set into memory.
//
// A set held only here would re-fire every memory on the next Telegram message,
// because the bridge builds a coordinator per message. That is the copy-per-call
// cost the general bool was added for, with the timing moved outside the run it
// was scoped to. Union rather than replace, so an absent field means "nothing
// delivered yet" and never a silent reset.
func (s *sidekickState) adoptDelivered(sess *sessionstore.Session) {
	if sess == nil {
		return
	}
	s.remember(sess.MemoryDelivered...)
}

// markDelivered records what reached the transcript, in memory and on the session.
//
// The write-through is what makes the scope a session rather than a run: the runner
// saves the session straight after annotating a result, so these ids are already in
// the file the next run loads. Nothing else has to remember them.
func (s *sidekickState) markDelivered(sess *sessionstore.Session, delivered []memory.Brief) {
	ids := make([]string, 0, len(delivered))
	for _, item := range delivered {
		if _, seen := s.delivered[item.ID]; seen {
			continue
		}
		ids = append(ids, item.ID)
	}
	s.remember(ids...)
	s.persist(sess, ids...)
}

// remember adds ids to the in-memory set.
func (s *sidekickState) remember(ids ...string) {
	for _, id := range ids {
		if id != "" {
			s.delivered[id] = struct{}{}
		}
	}
}

// persist carries ids onto the session file, the only half of the set that outlives
// a run.
func (s *sidekickState) persist(sess *sessionstore.Session, ids ...string) {
	if sess == nil || len(ids) == 0 {
		return
	}
	for _, id := range ids {
		if id != "" {
			sess.MemoryDelivered = append(sess.MemoryDelivered, id)
		}
	}
	if len(sess.MemoryDelivered) > sidekickDeliveredKeep {
		sess.MemoryDelivered = sess.MemoryDelivered[len(sess.MemoryDelivered)-sidekickDeliveredKeep:]
	}
}

// forgetDelivered empties the set on both sides.
//
// This is the seam a compaction epoch takes. A compaction drops the tool results
// that carried the notes, so the memory behind a note is no longer in context and
// delivering it once more is not a repeat. Both halves go: the persisted list is
// folded back in on the next call and would otherwise resurrect everything the
// epoch meant to release.
func (s *sidekickState) forgetDelivered(sess *sessionstore.Session) {
	s.delivered = map[string]struct{}{}
	if sess != nil {
		sess.MemoryDelivered = nil
	}
}

// bondBlock is the bond for the run's first tool result, and "" on every later one.
//
// LiveBond re-reads the file on every call, so the flag is what makes the second
// call a non-call: rendering again mid-run would put two revisions of the same
// operator-maintained file in one transcript, and the model has no way to tell
// which one is current.
//
// The flag goes down only once something has been answered. A read failure is
// retried on the next result, because nothing was delivered and a lost bond is
// the failure this whole path was added to stop. An over-budget render is not
// retried, because the budget is a property of the caller's configuration rather
// than of the moment.
func (s *sidekickState) bondBlock() string {
	if s == nil || s.bond == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bondDelivered {
		return ""
	}
	rendered, err := s.bond()
	if err != nil {
		// Logged, because a bond that never arrives is otherwise indistinguishable
		// from an install that has no BOND.md - which is the failure being fixed.
		logger.Warn("Bond render failed for sidekick delivery: " + err.Error())
		return ""
	}
	rendered = strings.TrimSpace(rendered)
	if rendered != "" && len(rendered) > sidekickBondBytes {
		logger.Warn(fmt.Sprintf("Bond block is %d bytes, over the %d allowed for tool results, so it was not delivered", len(rendered), sidekickBondBytes))
		rendered = ""
	}
	// Empty counts as answered: the file was read and there was nothing in it,
	// which is a first run rather than a fault.
	s.bondDelivered = true
	s.report.BondInjected = len(rendered)
	return rendered
}

// belowGate counts what the gate held back, so a run that delivered nothing can be
// told apart from a run that had nothing worth delivering.
func belowGate(entries []memory.Entry, now time.Time) int {
	n := 0
	for _, entry := range entries {
		if memory.ScoreOf(entry, now) < memory.SidekickGate {
			n++
		}
	}
	return n
}

// targetsFor is every path the completed operation was about.
func (s *sidekickState) targetsFor(sess *sessionstore.Session, result operation.ResultEvent) []string {
	for _, op := range sess.Operations {
		if op.ID != result.OpID {
			continue
		}
		return s.targetsForSpec(op.SpecType, op.SpecArgs)
	}
	return nil
}

func (s *sidekickState) deliveryReport() memory.DeliveryReport {
	if s == nil {
		return memory.DeliveryReport{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.report
}
