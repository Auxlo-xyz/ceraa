package coordinator

import (
	"path/filepath"
	"strings"
	"sync"

	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
)

// sidekickDeliveredKeep bounds the delivered set carried on a session file.
//
// Every entry is a 32-character id, so the list is small beside the transcript it
// rides in, but nothing else on a session shrinks and a session outlives any
// single store. The oldest ids are the ones forgotten: forgetting one costs a
// single repeated delivery much later, which is the recoverable direction.
const sidekickDeliveredKeep = 512

// defaultMemoryTail is how many entries of the append-only log delivery reads when
// the caller has not said.
//
// Derived rather than picked. One note carries at most SidekickGeneralLimit
// standing entries plus SidekickTargetLimit targeted ones, and a run may write
// SidekickRunBytes worth of notes before its ceiling stops it, which is four
// blocks. So 28 is exactly what a run that delivers on every call can render:
// below it the tail becomes the binding constraint and a later note in the same
// run finds nothing it has not already delivered, and above it the surplus
// entries cannot be reached at all, because the run ceiling is spent.
//
// A count rather than a byte budget because the renderer's own ceilings are what
// bound what reaches the model: the tail decides what is eligible, and an entry
// that was never rendered was never paid for.
const defaultMemoryTail = (memory.SidekickGeneralLimit + memory.SidekickTargetLimit) *
	(memory.SidekickRunBytes / memory.SidekickBlockBytes)

// pathTokenTrim is the punctuation that surrounds a path inside prose, dropped
// before a token is offered to the scope matcher. The trimming stops at the
// characters a glob needs, so `*.go` survives as a pattern rather than losing its
// star.
const pathTokenTrim = "`\"'()[]{}<>,;:"

// sidekickState carries what tool-output delivery needs, and records what it
// delivered so the two modes can be told apart later.
type sidekickState struct {
	mu sync.Mutex
	// memFile is the append-only log this run reads what has been learned from.
	// It is the only source: a reader rather than a snapshot because the file on
	// disk is the source of truth, and a snapshot taken at construction diverges
	// from it silently in a long-lived daemon.
	memFile func() *memory.MemoryFile
	// tail is how many entries memFile is read for. Zero never reaches here;
	// useMemoryFile substitutes defaultMemoryTail.
	tail    int
	workDir string

	// delivered is every memory already annotated into this session's transcript,
	// keyed by the log entry's content-hash id.
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

	// runSpent is the bytes of memory note this run has already written into tool
	// results. Per run rather than per session because the ceiling is about
	// attention inside one piece of work, and a session is not one piece of work.
	runSpent int

	report memory.DeliveryReport
}

func newSidekickState(workDir string) *sidekickState {
	return &sidekickState{
		workDir:   workDir,
		delivered: map[string]struct{}{},
	}
}

// useMemoryFile makes the append-only log this state's source of what has been
// learned, replacing the store reader it was built with.
//
// Replacement rather than addition, because the store re-ranked itself against a
// wall clock on every request and that is the volatility the log exists to
// remove: delivering from both would keep the volatile half of the pair live
// while appearing to have retired it.
func (s *sidekickState) useMemoryFile(file func() *memory.MemoryFile, tail int) {
	if s == nil || file == nil {
		return
	}
	if tail <= 0 {
		tail = defaultMemoryTail
	}
	s.memFile, s.tail = file, tail
}

// annotate returns the sidekick block for one completed operation, or "" when
// nothing qualifies.
func (s *sidekickState) annotate(sess *sessionstore.Session, result operation.ResultEvent) string {
	if s == nil || s.memFile == nil {
		return ""
	}
	targets := s.targetsFor(sess, result)

	s.mu.Lock()
	defer s.mu.Unlock()
	// Folded in before the decision rather than seeded at construction: the runner
	// is built per run, and the session file is the only thing that outlives one.
	s.adoptDelivered(sess)

	targeted, general, held := s.selectFromLog(targets)
	if len(targeted) == 0 && len(general) == 0 {
		// No gate on this path: the log is not scored, so nothing sits above or
		// below a floor. A run that delivered nothing here reads as "the tail had
		// nothing for this operation", which is a different and true answer from
		// what a gate would report.
		s.report.Deduped += held
		return ""
	}
	return s.deliver(sess, targets, targeted, general)
}

// deliver renders the selection into the note and charges it to the run's byte
// ceiling, returning "" when nothing may be sent.
//
// Shared by both sources because everything from here down is about what was
// already decided rather than about where it came from: the render, the run
// ceiling, the delivered set and the report are one mechanism, and a second copy
// of it is a second thing to keep correct.
func (s *sidekickState) deliver(sess *sessionstore.Session, targets []string, targeted, general []memory.Brief) string {
	note := memory.Annotate(targets, targeted, general)
	if note == "" {
		return ""
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
		return ""
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
	return note
}

// memTail is the newest entries of the log, oldest first, newest last, or nil
// when there is nothing to read.
//
// An unreadable file is an empty tail rather than an error: the conversation is
// the source of truth and the log is an optimisation, so a run that cannot read
// it delivers nothing instead of failing a turn. The file itself makes that
// promise; this only guarantees a nil answer for a caller that configured no
// reader, so the delivery path never has to branch on it.
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

// fileEntry is one logged memory split into what delivery needs from it: the
// identity it is remembered by, the text the model reads, and the paths it names.
type fileEntry struct {
	id    string
	body  string
	paths []string
}

// logEntries is the parsed tail, oldest first.
func (s *sidekickState) logEntries() []fileEntry {
	tail := s.memTail()
	out := make([]fileEntry, 0, len(tail))
	for _, raw := range tail {
		if entry := parseLogEntry(raw); entry.body != "" {
			out = append(out, entry)
		}
	}
	return out
}

// parseLogEntry reads one logged memory.
//
// The date-and-session line the log writes above every entry is dropped rather
// than rendered: when the agent learned something is a fact about the agent, not
// about the thing, and a delivered line is a budget better spent on the claim.
// An entry with no such line - a hand-edited file, a one-line note - is the whole
// of its own body, so the split is on the newline rather than on the date.
func parseLogEntry(raw string) fileEntry {
	// The first line is the log's own header; the entry's body is everything under
	// it. An entry with no newline has no header to drop and is all body.
	body := strings.TrimSpace(raw)
	if line := strings.IndexByte(raw, '\n'); line >= 0 {
		body = strings.TrimSpace(raw[line+1:])
	}
	// Normalised before hashing so two entries that differ only in whitespace are
	// one memory rather than two that each get delivered once.
	normalised := strings.Join(strings.Fields(body), " ")
	return fileEntry{
		id:    "memfile:" + shortHash(normalised),
		body:  body,
		paths: pathsNamed(body),
	}
}

// matchesAny reports whether the paths this entry names cover any of the paths
// the operation touched.
func (e fileEntry) matchesAny(targets []string) bool {
	for _, scope := range e.paths {
		for _, target := range targets {
			if memory.MatchesScope(scope, target) {
				return true
			}
		}
	}
	return false
}

func (e fileEntry) brief() memory.Brief {
	brief := memory.Brief{ID: e.id, Content: e.body}
	if len(e.paths) > 0 {
		// Carried for the report rather than rendered: the note's heading names
		// the operation's own targets, and a scope the entry wrote for itself is
		// not one of them.
		brief.Applies = e.paths[0]
	}
	return brief
}

// selectFromLog picks the log entries this operation is about, and the standing
// entries this run has not carried yet.
//
// The ordering is the log's own: oldest first. Nothing re-ranks, which is the
// whole point of the source - a ranking would need a clock, and a clock is what
// made the block volatile. The most recent entries are the ones kept when the
// tail carries more than a note can hold, because a log that has moved on has
// newer things to say about the same path.
//
// held is how many entries matched but were already delivered, so a run where
// dedup did all the work still reads as activity.
func (s *sidekickState) selectFromLog(targets []string) (targeted, general []memory.Brief, held int) {
	entries := s.logEntries()
	var matched, standing []fileEntry
	for _, entry := range entries {
		switch {
		case len(entry.paths) == 0:
			// No path named, so it is a standing preference rather than something
			// about one file. It goes out on the run's first tool result whatever
			// that result was about, because "applies everywhere" is a claim about
			// scope and not about this call.
			standing = append(standing, entry)
		case len(targets) > 0 && entry.matchesAny(targets):
			matched = append(matched, entry)
		}
	}
	// Excluded before the limit, not after. The limit is the newest N, so
	// filtering afterwards would let entries delivered on earlier calls hide
	// fresher ones, and the newest entry would then never fire in this session at
	// all - dedup that silently retires the tail.
	fresh := make([]fileEntry, 0, len(matched))
	for _, entry := range matched {
		if s.alreadyDelivered(entry.id) {
			held++
			continue
		}
		fresh = append(fresh, entry)
	}
	if len(fresh) > memory.SidekickTargetLimit {
		fresh = fresh[len(fresh)-memory.SidekickTargetLimit:]
	}
	if !s.generalDelivered && len(standing) > memory.SidekickGeneralLimit {
		standing = standing[len(standing)-memory.SidekickGeneralLimit:]
	}
	for _, entry := range fresh {
		targeted = append(targeted, entry.brief())
	}
	if !s.generalDelivered {
		for _, entry := range standing {
			general = append(general, entry.brief())
		}
	}
	return targeted, general, held
}

// pathsNamed is every path-like token an entry names.
//
// A log entry is prose, so the scope it carries is the path it mentions rather
// than a field. This is deliberately the same shape of judgement pathsFromCommand
// makes - a token that has a separator in it, or a base name that carries an
// extension - and it has the same failure mode: a token this misses is delivered
// on a later operation rather than delivered wrongly.
func pathsNamed(text string) []string {
	var out []string
	for _, field := range strings.Fields(text) {
		token := strings.TrimRight(strings.Trim(field, pathTokenTrim), ".")
		if token == "" || strings.Contains(token, "://") {
			continue
		}
		if !isPathLike(token) {
			continue
		}
		out = append(out, token)
	}
	return out
}

// isPathLike is "this token names a file or a directory".
//
// A bare word is not a path, because "gradle" names a tool and a memory about a
// tool is a standing preference rather than something about one file. The scope
// matcher does the rest, including a bare filename as a base-name match.
func isPathLike(token string) bool {
	if strings.ContainsAny(token, `/\`) {
		return true
	}
	base := filepath.Base(token)
	return strings.Contains(base, ".") && hasAlphanumeric(base)
}

// alreadyDelivered is the exclusion predicate the log selector consults.
func (s *sidekickState) alreadyDelivered(id string) bool {
	_, ok := s.delivered[id]
	return ok
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

// seedDelivered is the locked half of Runner.SeedDeliveredMemory: the session's own
// record first, then the newest fold's judgement over it.
//
// Both halves, and the second one earns its place on the case where the first is
// empty and the context is not. A fork copies the parent's messages and not its
// MemoryDelivered (sessionstore.Store.Fork), so a forked session arrives carrying
// every checkpoint its parent folded - and therefore carrying the notes those folds
// decided to keep - with a delivered list that says nothing was told. Reading the
// fold's set is what stops those from being announced a second time to a context
// that already holds them.
func (s *sidekickState) seedDelivered(sess *sessionstore.Session) {
	s.adoptDelivered(sess)
	s.carryFromCheckpoint(sess)
}

// carryFromCheckpoint folds a fold's own account of what it left represented into
// the delivered set, on both halves.
//
// Union, never replace. See Runner.SeedDeliveredMemory for why a replace here
// cannot express expiry even though it looks like it can; the short form is that
// the persisted list has no delivery time, so a fold's answer and the list can only
// be added together without also releasing what arrived after the fold.
//
// Persisted only for the ids the session list does not already hold. persist
// appends unconditionally and a seed runs once per message, so appending a
// survivor the list already carries would grow the file by the size of the fold's
// set on every message, until the 512-id cap forgets something that was still
// wanted.
func (s *sidekickState) carryFromCheckpoint(sess *sessionstore.Session) {
	carried := sess.LatestCheckpointDelivered()
	if len(carried) == 0 {
		return
	}
	s.remember(carried...)
	s.persistMissing(sess, carried...)
}

// reseedFromCheckpoint is the locked half of
// Runner.ReseedDeliveredMemoryFromCheckpoint: the fold's judgement replaces the
// session's list instead of joining it.
//
// An empty fold set is silence, not a decision to release everything.
// LatestCheckpointDelivered answers nil both for a session that has never folded
// and for one stored before the field existed, and reading that as "release
// everything" would reset a session that has delivered anything at all. A caller
// that means it says so with Runner.ForgetDeliveredMemory, which is explicit.
func (s *sidekickState) reseedFromCheckpoint(sess *sessionstore.Session) {
	carried := sess.LatestCheckpointDelivered()
	if len(carried) == 0 {
		return
	}
	s.forgetDelivered(sess)
	s.remember(carried...)
	s.persistMissing(sess, carried...)
}

// persistMissing appends the ids the session does not already record.
//
// The dedup lives here rather than in persist because persist is on the delivery
// path, where markDelivered has already filtered against the in-memory set, and
// changing it there would alter a path the running note tests already pin. Here
// the filter is the point: seeding repeats per message and appending is not
// idempotent.
func (s *sidekickState) persistMissing(sess *sessionstore.Session, ids ...string) {
	if sess == nil {
		return
	}
	held := make(map[string]struct{}, len(sess.MemoryDelivered))
	for _, id := range sess.MemoryDelivered {
		held[id] = struct{}{}
	}
	missing := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, seen := held[id]; seen {
			continue
		}
		held[id] = struct{}{}
		missing = append(missing, id)
	}
	s.persist(sess, missing...)
}

// SeedDeliveredMemory adopts the delivered set a session already carries, so a
// runner built for this message starts from the session's own record rather than
// from empty.
//
// The runner and its sidekick state are built per Telegram message, so an unseeded
// one re-fires every targeted memory on every message that follows. That is the
// copy-per-call cost the delivered set was built to stop, arriving one message
// later, and this is the single call that stops it. Call it once per run, after the
// session is loaded and recovered and before anything is annotated. It is not
// required: annotate folds the session's own list in on every call regardless, so
// seeding is what moves that work off the delivery path and adds the fold's half.
//
// Nil in, nil out. A runner in prompt or cached mode has no sidekick state and
// nothing to seed; a nil session has nothing to read; a session stored before
// either field existed reads as nothing delivered and delivers normally. An absent
// field is never a reset, which is why there is no error to return.
//
// Neither half of the seed expires anything, and that is the decision rather than
// an omission. Session.MemoryDelivered says an id reached the transcript; a fold
// deletes the tool results carrying those notes, so after a fold the list
// over-reports and the newest checkpoint's Delivered is the fold's own account of
// what survived, written while the notes were still on the wire. Replacing the
// list with it is the obvious move and it cannot work: the list records no delivery
// time, so list-minus-fold cannot be split into "delivered before the fold and
// dropped by it" and "delivered after the fold and still in context". A replace
// drops both, and drops the second kind again on every later run, because the
// newest checkpoint never learns about them. That converts a per-session dedup
// into a per-fold one - every memory delivered since the last fold re-sent on every
// message from there on - which is the repeat this mechanism exists to prevent, and
// it is worse than what an unseeded runner does today.
//
// So expiry belongs at the fold, where the evidence exists: ForgetDeliveredMemory
// on the near side and MarkDeliveredMemory with the Carry hook's answer on the far
// side. A dropped note and a surviving note are only distinguishable there, and
// Runner.ForgetDeliveredMemory is already the locked entry point for it - no
// state-level Reset is added, because sidekickState is unexported and a reset
// reachable only from this file would be a second name for a method the package
// already has. A caller that wants the other election gets
// ReseedDeliveredMemoryFromCheckpoint.
func (r *Runner) SeedDeliveredMemory(sess *sessionstore.Session) {
	if r.sidekick == nil {
		return
	}
	r.sidekick.mu.Lock()
	defer r.sidekick.mu.Unlock()
	r.sidekick.seedDelivered(sess)
}

// ReseedDeliveredMemoryFromCheckpoint replaces the session's delivered list with
// the newest fold's judgement instead of adding to it, so an id whose tool result
// that fold deleted is offered again.
//
// The aggressive half of the pair, kept as a named choice rather than folded into
// SeedDeliveredMemory, because which one is right is not a technical question and
// therefore must not be decided by the order two statements happen to sit in. What
// survives a fold is a judgement only the holder of the memory store can make, and
// the harness has already written that judgement down: a compaction drops the tool
// results that carried the notes, so delivering one again is not a repeat. A caller
// wiring the Compactor.Carry hook owns that decision, and this is where it reads
// its own answer through.
//
// What it costs, in daylight rather than implied: it forgets the whole list, so it
// also releases every id delivered after that fold, and it does so on every run
// until the next one, because the newest checkpoint keeps saying the same thing.
// Those ids are marked again as they are delivered, so the damage is one duplicate
// note per message for everything delivered since the last fold - which is the
// repeat the delivered set exists to prevent, and not the unbounded flood a run
// ceiling would suggest it might be. The recoverable direction is still the
// duplicate: a note that repeats costs context, a note that never arrives costs the
// agent the memory.
//
// A fold carrying nothing is read as silence rather than as a release. nil from
// LatestCheckpointDelivered means "this session has never said" as much as it means
// "nothing survived", and the first reading is a first run. The caller that wants a
// release says so with ForgetDeliveredMemory, which is unambiguous about doing it.
func (r *Runner) ReseedDeliveredMemoryFromCheckpoint(sess *sessionstore.Session) {
	if r.sidekick == nil {
		return
	}
	r.sidekick.mu.Lock()
	defer r.sidekick.mu.Unlock()
	r.sidekick.reseedFromCheckpoint(sess)
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
