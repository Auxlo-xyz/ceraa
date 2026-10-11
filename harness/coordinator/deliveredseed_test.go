package coordinator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// The seed is the entry point a runner calls once per run. Everything below drives
// it the way the bridge will - a fresh runner, a loaded session, one call before
// anything is annotated - because the claim is about a per-message coordinator, and
// a test that shared one coordinator would not be testing it.

// seed bodies are two targeted memories on two paths, so a test can tell
// suppression apart from a log that has nothing to say. One path silenced by a
// seed must leave the other delivering.
const (
	seedAPIBody  = "SEED_API: /srv/api pins jdk 21"
	seedWebBody  = "SEED_WEB: /srv/web builds on node 22"
	seedDarkBody = "SEED_DARK: prefers dark mode"
)

// The ids are the log's own content hashes, deterministic on the body text, so
// every runner built below remembers the same entry by the same id.
var (
	seedAPIID = parseLogEntry(seedAPIBody).id
	seedWebID = parseLogEntry(seedWebBody).id
)

// seededRunner is a runner in sidekick mode over a fixed log, built the way the
// bridge builds one: per message, with nothing delivered yet.
func seededRunner(t *testing.T, extra ...string) *Runner {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Memory.md")
	appendLog(t, memory.OpenMemoryFile(path), append([]string{seedAPIBody, seedWebBody}, extra...)...)
	r := &Runner{cfg: RunnerConfig{SessionID: "seed"}, reg: tool.DefaultRegistry()}
	r.delivery = memory.DeliverySidekick
	r.sidekick = newSidekickState("/srv")
	r.sidekick.useMemoryFile(logMemoryFile(path), 0)
	return r
}

// foldSession is a session with one standing fold carrying the given ids: the
// record of what that fold left represented, which is the only place a note's
// survival is written down.
func foldSession(ids ...string) *sessionstore.Session {
	sess := &sessionstore.Session{ID: "seed", Version: 3}
	sess.Messages = append(sess.Messages, sessionstore.Message{
		Role:       llm.RoleSystem,
		Compaction: &sessionstore.CompactionInfo{Summary: "fold", Seq: 1, Delivered: ids},
	})
	return sess
}

func hasDelivered(s *sidekickState, id string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.delivered[id]
	return ok
}

func heldIDs(sess *sessionstore.Session) []string {
	if sess == nil {
		return nil
	}
	return sess.MemoryDelivered
}

func joined(ids []string) string { return strings.Join(ids, ",") }

// The session's own record is the whole reason this call exists: the runner is
// built per Telegram message, so an unseeded one re-fires every targeted memory on
// the next message. The record has to be adopted, and suppressing it must not
// silence a memory that was never delivered.
func TestSeedingAdoptsTheSessionDeliveredSetSoAMatchingRuleIsNotRedelivered(t *testing.T) {
	r := seededRunner(t)
	sess := sessionWithOp("a", "write", "/srv/api/main.go")
	sess.MemoryDelivered = []string{seedAPIID}

	r.SeedDeliveredMemory(sess)

	if !hasDelivered(r.sidekick, seedAPIID) {
		t.Fatalf("the session's own delivered set was not adopted: %v", heldIDs(sess))
	}
	if note := r.sidekick.annotate(sess, operation.ResultEvent{OpID: "a"}); strings.Contains(note, "jdk 21") {
		t.Errorf("a memory the transcript already carries was annotated again: %q", note)
	}

	// The other path is different work, and deduping one id must not take the
	// other memory out of context with it.
	other := sessionWithOp("b", "write", "/srv/web/app.js")
	other.MemoryDelivered = []string{seedAPIID}
	if note := r.sidekick.annotate(other, operation.ResultEvent{OpID: "b"}); !strings.Contains(note, "node 22") {
		t.Errorf("seeding silenced a memory on a path it never covered: %q", note)
	}
}

// A fold's set is read as well, and this is the case where it is not redundant: a
// fork copies the parent's messages and not its delivered list, so a forked session
// arrives holding every note those folds decided to keep with nothing recorded as
// told. Without this half, the first run in a fork re-announces a context that
// already has the notes.
func TestSeedingAdoptsAFoldsCarriedSetSoItsSurvivorsAreNotRedelivered(t *testing.T) {
	r := seededRunner(t)
	sess := foldSession(seedAPIID)
	sess.Operations = append(sess.Operations, sessionstore.Operation{
		ID: "a", SpecType: "write", SpecArgs: map[string]any{"path": "/srv/api/main.go"},
	})

	r.SeedDeliveredMemory(sess)

	if !hasDelivered(r.sidekick, seedAPIID) {
		t.Fatalf("the fold's carried set was not adopted: %v", heldIDs(sess))
	}
	if note := r.sidekick.annotate(sess, operation.ResultEvent{OpID: "a"}); strings.Contains(note, "jdk 21") {
		t.Errorf("a memory the surviving checkpoint still carries was annotated again: %q", note)
	}
	// Written back onto the session, so the next message reads one record rather
	// than reaching for the fold again.
	if got := joined(heldIDs(sess)); got != seedAPIID {
		t.Errorf("session delivered set = %s, want the fold's own id", got)
	}
}

// Nothing recorded means nothing suppressed. A first run has to look exactly like a
// run with no seed at all, or the fix for repeat delivery becomes a way of losing
// memory delivery entirely.
func TestASessionWithNeitherDeliveredFieldIsSeededEmptyAndDeliversNormally(t *testing.T) {
	r := seededRunner(t)
	sess := sessionWithOp("a", "write", "/srv/api/main.go")

	r.SeedDeliveredMemory(sess)

	if len(r.sidekick.delivered) != 0 {
		t.Errorf("seeding armed %d ids out of an empty session", len(r.sidekick.delivered))
	}
	if note := r.sidekick.annotate(sess, operation.ResultEvent{OpID: "a"}); !strings.Contains(note, "jdk 21") {
		t.Errorf("an empty session stopped delivering: %q", note)
	}
	if got := joined(heldIDs(sess)); got != seedAPIID {
		t.Errorf("session delivered set = %s, want the delivered id recorded", got)
	}
}

// The fields are omitempty and a session file predating them has to read as "no
// ids", not as a failure and not as a reset. Driven through a real store and a real
// file with the keys simply absent, because that is the shape an old session has.
func TestASessionStoredBeforeTheDeliveredFieldsReadsAsEmptyNotAsAFailure(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"id":"legacy","version":3,` +
		`"messages":[{"role":"system","content":"system",` +
		`"compaction":{"summary":"a fold from before the field","seq":1}}],` +
		`"operations":[{"id":"a","spec_type":"write","status":"completed",` +
		`"spec_args":{"path":"/srv/api/main.go"}}]}`
	if err := os.WriteFile(filepath.Join(dir, "legacy.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Load("legacy")
	if err != nil {
		t.Fatal(err)
	}

	r := seededRunner(t)
	r.SeedDeliveredMemory(sess)

	if len(r.sidekick.delivered) != 0 {
		t.Errorf("a session stored before the fields seeded %v", r.sidekick.delivered)
	}
	if note := r.sidekick.annotate(sess, operation.ResultEvent{OpID: "a"}); !strings.Contains(note, "jdk 21") {
		t.Errorf("a legacy session stopped delivering, which is what reading an absent key as a reset looks like: %q", note)
	}
}

// The ordering decision, pinned by intent: seeding does NOT expire.
//
// The fold deleted the tool result that carried id-api and kept id-web, so the
// session's list over-reports by exactly id-api. Reading the fold as a release
// would need to subtract it - and with no delivery time on the list, that
// subtraction also releases everything delivered after the fold, on every run, and
// turns a per-session dedup into a per-fold one. So the union stands: the seed adds
// the fold's judgement and releases nothing.
func TestSeedingNeverReOffersAnIdWhoseToolResultTheFoldDeleted(t *testing.T) {
	r := seededRunner(t)
	sess := foldSession(seedWebID)
	sess.MemoryDelivered = []string{seedAPIID, seedWebID}
	sess.Operations = append(sess.Operations, sessionstore.Operation{
		ID: "a", SpecType: "write", SpecArgs: map[string]any{"path": "/srv/api/main.go"},
	})

	r.SeedDeliveredMemory(sess)

	if !hasDelivered(r.sidekick, seedAPIID) {
		t.Errorf("seeding expired id-api, whose tool result the fold deleted: %v", heldIDs(sess))
	}
	if note := r.sidekick.annotate(sess, operation.ResultEvent{OpID: "a"}); strings.Contains(note, "jdk 21") {
		t.Errorf("seeding re-offered the dropped note: %q", note)
	}
	if got := joined(heldIDs(sess)); got != seedAPIID+","+seedWebID {
		t.Errorf("session delivered set = %s, want the union with nothing dropped", got)
	}
}

// The aggressive variant is the same decision taken the other way, so its cost is
// asserted rather than described: an id the fold deleted IS offered again, and the
// session list becomes the fold's judgement and nothing else.
//
// Named for what it does, because a test that only asserted the safe direction would
// leave the aggressive path unmeasured and therefore free to drift.
func TestTheAggressiveReseedReOffersAnIdWhoseToolResultTheFoldDeleted(t *testing.T) {
	r := seededRunner(t)
	sess := foldSession(seedWebID)
	sess.MemoryDelivered = []string{seedAPIID, seedWebID}
	sess.Operations = append(sess.Operations, sessionstore.Operation{
		ID: "a", SpecType: "write", SpecArgs: map[string]any{"path": "/srv/api/main.go"},
	})

	r.ReseedDeliveredMemoryFromCheckpoint(sess)

	if got := joined(heldIDs(sess)); got != seedWebID {
		t.Errorf("session delivered set = %s, want the fold's judgement alone", got)
	}
	note := r.sidekick.annotate(sess, operation.ResultEvent{OpID: "a"})
	if !strings.Contains(note, "jdk 21") {
		t.Errorf("the aggressive reseed did not release the dropped note, so it is the same function as the seed: %q", note)
	}
	// And it is still deduping: a memory the fold kept stays out of context.
	if repeat := r.sidekick.annotate(sessionWithOp("b", "write", "/srv/api/main.go"), operation.ResultEvent{OpID: "b"}); strings.Contains(repeat, "jdk 21") {
		t.Errorf("the released note repeated inside one run: %q", repeat)
	}
	web := r.sidekick.annotate(sessionWithOp("c", "write", "/srv/web/app.js"), operation.ResultEvent{OpID: "c"})
	if strings.Contains(web, "node 22") {
		t.Errorf("a memory the fold kept was re-offered: %q", web)
	}
}

// Silence is not a decision to release. LatestCheckpointDelivered answers nil both
// for a session that has never folded and for one stored before the field, so
// forgetting on nil would reset a session that has delivered everything it has - the
// repeat the delivered set exists to prevent, arrived at through the fix for it.
func TestAReseedWithNoStandingCheckpointForgetsNothing(t *testing.T) {
	r := seededRunner(t)
	sess := sessionWithOp("a", "write", "/srv/api/main.go")
	sess.MemoryDelivered = []string{seedAPIID}

	// Seed first, as every real run does: the session file is what the sidekick
	// state adopts, and a reseed with nothing loaded has nothing to preserve.
	r.SeedDeliveredMemory(sess)
	if !hasDelivered(r.sidekick, seedAPIID) {
		t.Fatal("the seed did not adopt the session's delivered id, so the reseed below proves nothing")
	}
	r.ReseedDeliveredMemoryFromCheckpoint(sess)

	if got := joined(heldIDs(sess)); got != seedAPIID {
		t.Errorf("session delivered set = %s, want the delivered id untouched", got)
	}
	if !hasDelivered(r.sidekick, seedAPIID) {
		t.Error("a reseed with nothing to reseed from dropped a delivered id")
	}
	if note := r.sidekick.annotate(sess, operation.ResultEvent{OpID: "a"}); strings.Contains(note, "jdk 21") {
		t.Errorf("the memory came back with no fold to explain it: %q", note)
	}
}

// A seed runs once per message on the same session file, so it has to be idempotent.
// persist appends unconditionally; a seed that appended what the session already
// held would grow the file by the fold's set on every message until the 512-id cap
// forgets something still wanted.
func TestSeedingTheSameFoldOnEveryMessageDoesNotGrowTheSessionDeliveredList(t *testing.T) {
	r := seededRunner(t)
	sess := foldSession(seedAPIID, seedWebID)

	for i := 0; i < 5; i++ {
		r.SeedDeliveredMemory(sess)
		if len(r.sidekick.delivered) != 2 {
			t.Fatalf("message %d armed %d ids, want 2", i, len(r.sidekick.delivered))
		}
	}
	if got := joined(heldIDs(sess)); got != seedAPIID+","+seedWebID {
		t.Errorf("session delivered set = %s, want the fold's ids once", got)
	}
}

// The once-per-run flag is not the seed's business. A seed that armed it would
// drop this run's baseline on the strength of a previous run's notes, which is
// the failure the general set is deliberately scoped against: it has to survive
// exactly when the context carrying it was rewritten.
func TestSeedingDoesNotArmTheOncePerRunFlags(t *testing.T) {
	r := seededRunner(t, seedDarkBody)
	sess := foldSession(seedAPIID)
	sess.MemoryDelivered = []string{seedAPIID}
	sess.Operations = append(sess.Operations, sessionstore.Operation{
		ID: "a", SpecType: "write", SpecArgs: map[string]any{"path": "/srv/api/main.go"},
	})

	r.SeedDeliveredMemory(sess)

	if r.sidekick.generalDelivered {
		t.Error("seeding armed the once-per-run flag, so this run would open without its baseline")
	}
	if note := r.sidekick.annotate(sess, operation.ResultEvent{OpID: "a"}); !strings.Contains(note, "dark mode") {
		t.Errorf("the standing preference was suppressed by a seed that carried no ids for it: %q", note)
	}
}

// A runner in prompt or cached mode has no sidekick state, so every delivered-set
// entry point is a no-op there. It has to be a whole no-op: a half-applied one
// would clear the session's list from a runner that cannot deliver anything, losing
// the record for a mode that is relying on it.
func TestSeedingIsSafeWithNoSidekickStateAndOnAnEmptySession(t *testing.T) {
	detached := &Runner{cfg: RunnerConfig{SessionID: "seed"}}
	sess := sessionWithOp("a", "write", "/srv/api/main.go")
	sess.MemoryDelivered = []string{seedAPIID}

	detached.SeedDeliveredMemory(sess)
	detached.ReseedDeliveredMemoryFromCheckpoint(sess)
	detached.ForgetDeliveredMemory(sess)
	detached.MarkDeliveredMemory(sess, seedWebID)

	if got := joined(heldIDs(sess)); got != seedAPIID {
		t.Errorf("a runner with no sidekick state changed the session's record: %s", got)
	}

	// A nil session and an empty one are both first runs, and neither may arm
	// anything or panic on the way there.
	r := seededRunner(t)
	r.SeedDeliveredMemory(nil)
	r.SeedDeliveredMemory(&sessionstore.Session{})
	r.ReseedDeliveredMemoryFromCheckpoint(nil)
	if len(r.sidekick.delivered) != 0 {
		t.Errorf("seeding nothing armed %v", r.sidekick.delivered)
	}
}
