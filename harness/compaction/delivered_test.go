package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

// A checkpoint carries the delivered ids it judged still standing. The bug this
// exists for is silent in both directions: the session's own delivered set says
// the model was told X, the fold deleted the only tool result that said it, and
// nothing errors - the agent simply never asks again. Every test here is a
// claim that a fold cannot quietly lose an id.

func checkpoint(summary string, ids ...string) sessionstore.Message {
	return sessionstore.Message{
		Role:    llm.RoleSystem,
		Content: "<conversation-checkpoint>",
		Compaction: &sessionstore.CompactionInfo{
			Summary:    summary,
			Tier:       1,
			Generation: "young",
			Delivered:  ids,
		},
	}
}

func oldCheckpoint(summary string, ids ...string) sessionstore.Message {
	message := checkpoint(summary, ids...)
	message.Compaction.Generation = "old"
	message.Compaction.Survived = 9
	return message
}

// newestCheckpoint returns the last checkpoint on the wire, which is the one a
// runner reads back after Recover.
func newestCheckpoint(t *testing.T, sess *sessionstore.Session) *sessionstore.CompactionInfo {
	t.Helper()
	for i := len(sess.Messages) - 1; i >= 0; i-- {
		if sess.Messages[i].Compaction != nil {
			return sess.Messages[i].Compaction
		}
	}
	t.Fatal("no checkpoint on the session; nothing was folded")
	return nil
}

func deliveredCompactor(t *testing.T, replies ...string) (*Compactor, func()) {
	t.Helper()
	if len(replies) == 0 {
		replies = []string{validTestSummary()}
	}
	srv := newSummaryServer(t, replies...)
	c := inBandCompactor(srv.server, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fold"}})
	c.Settings.Keep.Tokens = 8000
	return c, srv.close
}

// sessionWithCheckpoints puts the given checkpoints ahead of a foldable tail.
func sessionWithCheckpoints(id string, leading []sessionstore.Message) *sessionstore.Session {
	sess := foldTestSession(10)
	sess.ID = id
	return &sessionstore.Session{ID: id, Messages: append(leading, sess.Messages...)}
}

// A fold that deletes the tool results behind an id must carry that id onto
// the checkpoint it writes. Without the carry the delivered set outlives the
// transcript that justified it and the model is never told again.
func TestDeliveredIdsSurviveAFold(t *testing.T) {
	c, closeServer := deliveredCompactor(t)
	defer closeServer()
	c.Carry = func(_ *sessionstore.Session, previous []string) []string { return previous }
	sess := sessionWithCheckpoints("carry-fold", []sessionstore.Message{
		checkpoint("prior work", "id-api", "id-style"),
	})
	ok, err := c.Compact(context.Background(), sess)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !ok {
		t.Fatal("nothing was folded")
	}
	got := newestCheckpoint(t, sess)
	if strings.Contains(got.Summary, "msg-01") {
		t.Fatalf("the test did not fold: the checkpoint still carries raw messages: %.120q", got.Summary)
	}
	want := []string{"id-api", "id-style"}
	if len(got.Delivered) != len(want) {
		t.Fatalf("checkpoint delivered = %v, want %v", got.Delivered, want)
	}
	for i, id := range want {
		if got.Delivered[i] != id {
			t.Errorf("checkpoint delivered[%d] = %q, want %q (full %v)", i, got.Delivered[i], id, got.Delivered)
		}
	}
	// The pre-fold copies really are gone, which is the whole reason the carry
	// exists. A test that passed with the messages still present would be
	// asserting nothing.
	if strings.Contains(sessionContents(sess), "msg-01 task body") {
		t.Error("the folded range survived; the carry would be untested if it did")
	}
}

// The hook is the judgement and the compactor is the courier: it hands over
// exactly what the owner returned, including a set that is not simply the
// previous one. A compactor that second-guessed the answer would be deciding
// what an id means, which it cannot know.
func TestTheCarryHookDecidesAndSeesEveryStandingSet(t *testing.T) {
	c, closeServer := deliveredCompactor(t)
	defer closeServer()
	var seen []string
	calls := 0
	c.Carry = func(sess *sessionstore.Session, previous []string) []string {
		calls++
		seen = append([]string{}, previous...)
		if sess == nil {
			t.Error("the hook was handed no session; it cannot read the delivered list without one")
		}
		return []string{"id-api", "id-fresh"}
	}
	sess := sessionWithCheckpoints("carry-decides", []sessionstore.Message{
		checkpoint("older work", "id-api"),
		checkpoint("prior work", "id-style"),
	})
	if _, err := c.Compact(context.Background(), sess); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if calls != 1 {
		t.Fatalf("carry hook called %d times, want 1 per fold", calls)
	}
	if len(seen) != 2 || seen[0] != "id-api" || seen[1] != "id-style" {
		t.Errorf("previous = %v, want the union of every standing checkpoint's set", seen)
	}
	got := newestCheckpoint(t, sess).Delivered
	if len(got) != 2 || got[0] != "id-api" || got[1] != "id-fresh" {
		t.Errorf("checkpoint delivered = %v, want exactly what the hook returned [id-api id-fresh]", got)
	}
}

// nil Carry forwards the previous set unchanged. That is the safe direction:
// nothing expires silently, and a caller with no sidekick has an empty set to
// forward, which is the same as carrying nothing.
func TestAFoldWithNoCarryHookForwardsThePreviousSet(t *testing.T) {
	c, closeServer := deliveredCompactor(t)
	defer closeServer()
	if c.Carry != nil {
		t.Fatal("this compactor has a Carry hook; the nil default is what is under test")
	}
	sess := sessionWithCheckpoints("carry-nil", []sessionstore.Message{
		checkpoint("prior work", "id-api", "id-style", "id-ci"),
	})
	if _, err := c.Compact(context.Background(), sess); err != nil {
		t.Fatalf("compact: %v", err)
	}
	got := newestCheckpoint(t, sess).Delivered
	if len(got) != 3 || got[0] != "id-api" || got[1] != "id-style" || got[2] != "id-ci" {
		t.Errorf("checkpoint delivered = %v, want the previous set forwarded unchanged", got)
	}
}

// mergeTier rebuilds N checkpoints as one and drops every field it does not
// name. Tested directly rather than through Compact, because through Compact
// the fold that runs afterwards papers over the loss: the ids come back from
// the standing checkpoints that mergeTier left alone, so a missing union here
// only shows up several compactions later with nothing in the log.
func TestTierMergeCarriesDeliveredIdsOntoTheMergedBlock(t *testing.T) {
	c, closeServer := deliveredCompactor(t)
	defer closeServer()
	sess := &sessionstore.Session{ID: "merge-delivered"}
	sess.Messages = append(sess.Messages,
		oldCheckpoint("ancient work one", "id-api"),
		oldCheckpoint("ancient work two", "id-style", "id-api"),
		sessionstore.Message{Role: llm.RoleUser, Content: "recent task"},
	)
	c.mergeTier(context.Background(), sess, 1, Tier2Trigger)

	var blocks []sessionstore.Message
	for _, message := range sess.Messages {
		if message.Compaction != nil {
			blocks = append(blocks, message)
		}
	}
	if len(blocks) != 1 {
		t.Fatalf("checkpoints after the merge = %d, want 1 replacement block", len(blocks))
	}
	if got := tierOf(blocks[0].Compaction); got != 2 {
		t.Errorf("merged block tier = %d, want 2", got)
	}
	want := []string{"id-api", "id-style"}
	got := blocks[0].Compaction.Delivered
	if len(got) != len(want) {
		t.Fatalf("merged block delivered = %v, want the union of the consumed blocks %v", got, want)
	}
	for i, id := range want {
		if got[i] != id {
			t.Errorf("merged delivered[%d] = %q, want %q (full %v)", i, got[i], id, got)
		}
	}
	if strings.Contains(blocks[0].Compaction.Summary, "ancient work") {
		t.Error("the consumed summaries did not feed the merge")
	}
	if sess.Messages[1].Content != "recent task" {
		t.Errorf("a message the merge did not consume moved: %+v", sess.Messages[1])
	}
}

// A merge fires inside Compact, ahead of the fold, so the fold's Carry hook sees
// the union rather than nothing. This is the composition that loses ids "a few
// compactions later" when mergeTier forgets them: the merged block is the only
// copy, the fold forwards its empty set, and every later fold inherits empty.
func TestDeliveredIdsSurviveAMergeAndTheFoldAfterIt(t *testing.T) {
	c, closeServer := deliveredCompactor(t, validTestSummary(), validTestSummary())
	defer closeServer()
	c.Carry = func(_ *sessionstore.Session, previous []string) []string { return previous }
	sess := &sessionstore.Session{ID: "merge-then-fold"}
	sess.Messages = append(sess.Messages,
		oldCheckpoint("ancient work one", "id-api"),
		oldCheckpoint("ancient work two", "id-style"),
	)
	for i := 0; i < 10; i++ {
		sess.Messages = append(sess.Messages, sessionstore.Message{
			Role: llm.RoleUser, Content: fmt.Sprintf("msg-%02d task body", i),
		})
	}
	ok, err := c.Compact(context.Background(), sess)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !ok {
		t.Fatal("nothing was folded")
	}
	if got := sess.CheckpointDelivered(); len(got) != 2 {
		t.Fatalf("standing delivered sets = %v, want both ids", got)
	}
	for _, id := range []string{"id-api", "id-style"} {
		found := false
		for _, m := range sess.Messages {
			if m.Compaction != nil && contains(m.Compaction.Delivered, id) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is on no checkpoint after a merge followed by a fold", id)
		}
	}
}

// No sidekick, no ids: the field must be absent from the stored bytes, not an
// empty array, or every session in the field grows a key that says nothing.
func TestASessionWithNoDeliveredIdsIsUnaffected(t *testing.T) {
	c, closeServer := deliveredCompactor(t)
	defer closeServer()
	calls := 0
	c.Carry = func(_ *sessionstore.Session, previous []string) []string {
		calls++
		if len(previous) != 0 {
			t.Errorf("the hook was handed %v from a session that delivered nothing", previous)
		}
		return previous
	}
	sess := foldTestSession(10)
	sess.ID = "no-sidekick"
	if _, err := c.Compact(context.Background(), sess); err != nil {
		t.Fatalf("compact: %v", err)
	}
	// The hook runs on every fold, empty set or not: asking costs nothing and
	// skipping the question on the one fold that could prove the set is empty
	// would be a special case that silently diverges.
	if calls != 1 {
		t.Errorf("the hook was called %d times, want 1 per fold", calls)
	}
	got := newestCheckpoint(t, sess)
	if len(got.Delivered) != 0 {
		t.Fatalf("a session with no sidekick grew delivered = %v", got.Delivered)
	}
	encoded, err := json.Marshal(sess)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "delivered") {
		t.Errorf("an empty set reached the session file, which omitempty exists to prevent: %.400s", encoded)
	}
}

// Absent is a legal stored state, not a corrupt one: a checkpoint written
// before this field decodes to an empty set and a fold over it does not fail.
func TestAStoredCheckpointWithoutTheFieldStillCompacts(t *testing.T) {
	stored := `{"id":"legacy","version":3,"messages":[` +
		`{"role":"system","content":"<conversation-checkpoint>","compaction":{"summary":"prior work","seq":1,"tier":1,"generation":"young"}},` +
		`{"role":"user","content":"keep going"},` +
		`{"role":"user","content":"and again"},` +
		`{"role":"user","content":"and again again"},` +
		`{"role":"user","content":"and again again again"},` +
		`{"role":"user","content":"and again again again again"},` +
		`{"role":"user","content":"and again again again again again"},` +
		`{"role":"user","content":"and again again again again again again"},` +
		`{"role":"user","content":"and again again again again again again again"},` +
		`{"role":"user","content":"and again again again again again again again again"},` +
		`{"role":"user","content":"and again again again again again again again again again"}` +
		`]}`
	var sess sessionstore.Session
	if err := json.Unmarshal([]byte(stored), &sess); err != nil {
		t.Fatalf("decode a session written before the field: %v", err)
	}
	if len(sess.CheckpointDelivered()) != 0 {
		t.Errorf("a legacy checkpoint reported delivered ids: %v", sess.CheckpointDelivered())
	}
	c, closeServer := deliveredCompactor(t)
	defer closeServer()
	var asked []string
	c.Carry = func(_ *sessionstore.Session, previous []string) []string {
		asked = append([]string{}, previous...)
		return previous
	}
	ok, err := c.Compact(context.Background(), &sess)
	if err != nil {
		t.Fatalf("compact over a session predating the field: %v", err)
	}
	if !ok {
		t.Fatal("nothing was folded")
	}
	if len(asked) != 0 {
		t.Errorf("the hook was handed %v from a session that carried none", asked)
	}
	if got := newestCheckpoint(t, &sess); len(got.Delivered) != 0 {
		t.Errorf("the new checkpoint invented delivered ids: %v", got.Delivered)
	}
}

// The round trip is the field's whole contract with the file: present stays
// present and in order, absent stays absent rather than becoming a nil that
// later code has to distinguish from an empty judgement.
func TestDeliveredRoundTripsThroughTheSessionFile(t *testing.T) {
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	sess := &sessionstore.Session{ID: "round-trip", Messages: []sessionstore.Message{
		checkpoint("carried", "id-api", "id-style"),
		{Role: llm.RoleUser, Content: "hello"},
	}}
	if err := store.Save(sess); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := store.Load("round-trip")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := loaded.CheckpointDelivered()
	if len(got) != 2 || got[0] != "id-api" || got[1] != "id-style" {
		t.Fatalf("round trip delivered = %v, want [id-api id-style]", got)
	}

	empty := &sessionstore.Session{ID: "round-trip-empty", Messages: []sessionstore.Message{
		checkpoint("no sidekick"),
	}}
	if err := store.Save(empty); err != nil {
		t.Fatalf("save empty: %v", err)
	}
	reloaded, err := store.Load("round-trip-empty")
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(reloaded.CheckpointDelivered()) != 0 {
		t.Errorf("an absent field decoded to something: %v", reloaded.CheckpointDelivered())
	}
}

// CheckpointDelivered is the union Compact hands the hook, so it is the union
// it must be: first-seen order, repeats dropped, blanks ignored.
func TestCheckpointDeliveredUnionsStandingSets(t *testing.T) {
	sess := &sessionstore.Session{ID: "union", Messages: []sessionstore.Message{
		checkpoint("one", "id-api", "", "id-style"),
		{Role: llm.RoleUser, Content: "work"},
		checkpoint("two", "id-style", "id-ci"),
	}}
	got := sess.CheckpointDelivered()
	want := []string{"id-api", "id-style", "id-ci"}
	if len(got) != len(want) {
		t.Fatalf("CheckpointDelivered = %v, want %v", got, want)
	}
	for i, id := range want {
		if got[i] != id {
			t.Errorf("CheckpointDelivered[%d] = %q, want %q (full %v)", i, got[i], id, got)
		}
	}
}

func contains(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}
