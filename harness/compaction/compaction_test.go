package compaction

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

func TestSelectMessageUnitsKeepsToolTurnWhole(t *testing.T) {
	messages := []sessionstore.Message{
		{Role: llm.RoleSystem, Content: "system"},
		{Role: llm.RoleUser, Content: "first task " + strings.Repeat("x", 400)},
		{Role: llm.RoleAssistant, ToolCalls: []sessionstore.ToolCallRef{{ID: "call_1", Name: "read", Args: `{"path":"a"}`}}},
		{Role: llm.RoleTool, ToolCallID: "call_1", ToolResult: "a"},
		{Role: llm.RoleUser, Content: "second task"},
		{Role: llm.RoleAssistant, ToolCalls: []sessionstore.ToolCallRef{{ID: "call_2", Name: "read", Args: `{"path":"b"}`}}},
		{Role: llm.RoleTool, ToolCallID: "call_2", ToolResult: "b"},
	}
	head, recent := selectMessageUnits(messages, 130)
	if len(head) != 3 {
		t.Fatalf("expected first unit in head, got %d messages", len(head))
	}
	if len(recent) != 3 {
		t.Fatalf("expected second unit in recent, got %d messages", len(recent))
	}
	if recent[1].AllToolCalls()[0].ID != "call_2" || recent[2].ToolCallID != "call_2" {
		t.Fatalf("recent unit split tool call boundary: %+v", recent)
	}
}

func foldTestSession(n int) *sessionstore.Session {
	messages := make([]sessionstore.Message, 0, n)
	for i := 0; i < n; i++ {
		messages = append(messages, sessionstore.Message{
			Role:    llm.RoleUser,
			Content: fmt.Sprintf("msg-%02d task body", i),
		})
	}
	return &sessionstore.Session{ID: "fold-test", Messages: messages}
}

func sessionContents(sess *sessionstore.Session) string {
	var b strings.Builder
	for _, m := range sess.Messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func countCheckpoints(sess *sessionstore.Session) int {
	n := 0
	for _, m := range sess.Messages {
		if m.Compaction != nil {
			n++
		}
	}
	return n
}

// A fold summarizes the oldest slice only: the newest forty percent of messages
// stay verbatim, and the checkpoint lands where the folded range was.
func TestFoldKeepsNewestFortyPercent(t *testing.T) {
	srv := newSummaryServer(t, validTestSummary())
	defer srv.close()
	c := inBandCompactor(srv.server, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fold"}})
	c.Settings.Keep.Tokens = 8000
	sess := foldTestSession(10)
	ok, err := c.Compact(context.Background(), sess)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !ok {
		t.Fatal("nothing was folded")
	}
	got := sessionContents(sess)
	// Newest 40% (msg-06 through msg-09) must survive verbatim.
	for _, want := range []string{"msg-06", "msg-07", "msg-08", "msg-09"} {
		if !strings.Contains(got, want) {
			t.Errorf("recent %q was folded; the newest forty percent must stay", want)
		}
	}
	// The first user message is pinned: it survives even though the folded
	// range covers it.
	if !strings.Contains(got, "msg-00 task body") {
		t.Error("msg-00 is gone; the first user message must survive every fold")
	}
	// Oldest folded range must be gone, replaced by exactly one checkpoint.
	for _, gone := range []string{"msg-01", "msg-02"} {
		if strings.Contains(got, gone) {
			t.Errorf("folded %q is still in the session; the fold must replace its range", gone)
		}
	}
	if n := countCheckpoints(sess); n != 1 {
		t.Fatalf("checkpoints = %d, want 1", n)
	}
	// The checkpoint sits where the fold was: first, with the kept tail after it.
	if sess.Messages[0].Compaction == nil {
		t.Error("the checkpoint is not at the folded range position")
	}
	tail := sessionContents(&sessionstore.Session{Messages: sess.Messages[len(sess.Messages)-4:]})
	for _, want := range []string{"msg-06", "msg-07", "msg-08", "msg-09"} {
		if !strings.Contains(tail, want) {
			t.Errorf("tail does not end with %q; the structural tail moved", want)
		}
	}
	if len(c.FoldLedger()) != 1 {
		t.Fatalf("ledger entries = %d, want 1 after one fold", len(c.FoldLedger()))
	}
}

// Two compactions extend the ledger instead of re summarizing: the second fold
// starts after the first checkpoint and appends a second entry.
func TestFoldLedgerGrowsAcrossTwoCompactions(t *testing.T) {
	srv := newSummaryServer(t, validTestSummary(), validTestSummary())
	defer srv.close()
	c := inBandCompactor(srv.server, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fold"}})
	c.Settings.Keep.Tokens = 8000
	sess := foldTestSession(10)
	if _, err := c.Compact(context.Background(), sess); err != nil {
		t.Fatalf("first compact: %v", err)
	}
	first := c.FoldLedger()
	if len(first) != 1 {
		t.Fatalf("ledger entries after one fold = %d, want 1", len(first))
	}
	if first[0].Start >= first[0].End {
		t.Fatalf("ledger range is empty: %+v", first[0])
	}
	if first[0].ID == "" || first[0].At.IsZero() {
		t.Fatalf("ledger entry misses id or timestamp: %+v", first[0])
	}
	sess.Messages = append(sess.Messages,
		sessionstore.Message{Role: llm.RoleUser, Content: "msg-10 task body"},
		sessionstore.Message{Role: llm.RoleUser, Content: "msg-11 task body"},
	)
	ok, err := c.Compact(context.Background(), sess)
	if err != nil {
		t.Fatalf("second compact: %v", err)
	}
	if !ok {
		t.Fatal("second fold did nothing; it should extend past the first checkpoint")
	}
	second := c.FoldLedger()
	if len(second) != 2 {
		t.Fatalf("ledger entries after two folds = %d, want 2", len(second))
	}
	if second[0].ID == second[1].ID {
		t.Errorf("ledger ids did not advance: %+v", second)
	}
	if n := countCheckpoints(sess); n != 2 {
		t.Fatalf("checkpoints = %d, want 2 after two folds", n)
	}
	got := sessionContents(sess)
	for _, want := range []string{"msg-10", "msg-11"} {
		if !strings.Contains(got, want) {
			t.Errorf("recent %q was folded on the second pass", want)
		}
	}
}

// The first user message is pinned: a fold covering it keeps it verbatim
// right after the checkpoint, while the rest of its range is replaced.
func TestFirstUserMessagePinSurvivesFold(t *testing.T) {
	srv := newSummaryServer(t, validTestSummary())
	defer srv.close()
	c := inBandCompactor(srv.server, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fold"}})
	c.Settings.Keep.Tokens = 8000
	sess := foldTestSession(10)
	ok, err := c.Compact(context.Background(), sess)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !ok {
		t.Fatal("nothing was folded")
	}
	if sess.Messages[0].Compaction == nil {
		t.Fatal("no checkpoint at the folded range position")
	}
	if len(sess.Messages) < 2 || sess.Messages[1].Content != "msg-00 task body" {
		t.Fatalf("pinned first user message is not right after the checkpoint: %+v", sess.Messages[1])
	}
	if n := countCheckpoints(sess); n != 1 {
		t.Fatalf("checkpoints = %d, want 1", n)
	}
}

// Survival ticks promote checkpoints to old age: five compactions survived
// flips young to old, which is what makes a block merge eligible.
func TestTickSurvivorsPromotesAtFive(t *testing.T) {
	messages := []sessionstore.Message{
		{Role: llm.RoleSystem, Content: "<conversation-checkpoint>", Compaction: &sessionstore.CompactionInfo{Summary: "old work"}},
	}
	for i := 0; i < 4; i++ {
		tickSurvivors(messages)
		if generationOf(messages[0].Compaction) != "young" {
			t.Fatalf("promoted after %d ticks, want young until five", i+1)
		}
	}
	tickSurvivors(messages)
	if generationOf(messages[0].Compaction) != "old" {
		t.Fatal("still young after five ticks; promotion never fired")
	}
	if messages[0].Compaction.Survived != 5 {
		t.Fatalf("survived = %d, want 5", messages[0].Compaction.Survived)
	}
}

// Tier distillation merges old tier-1 checkpoints into one tier-2 block while
// a compaction is already running, and the fresh fold still lands at tier 1.
func TestTierMergeDistillsOldTierOnes(t *testing.T) {
	srv := newSummaryServer(t, validTestSummary(), validTestSummary())
	defer srv.close()
	c := inBandCompactor(srv.server, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fold"}})
	c.Settings.Keep.Tokens = 8000
	old := func(summary string) sessionstore.Message {
		return sessionstore.Message{
			Role:    llm.RoleSystem,
			Content: "<conversation-checkpoint>",
			Compaction: &sessionstore.CompactionInfo{
				Summary: summary, Tier: 1, Generation: "old", Survived: 9,
			},
		}
	}
	sess := &sessionstore.Session{ID: "tier-test"}
	sess.Messages = append(sess.Messages, old("ancient work one"), old("ancient work two"))
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
	var tiers []int
	for _, message := range sess.Messages {
		if message.Compaction == nil {
			continue
		}
		tiers = append(tiers, tierOf(message.Compaction))
		if strings.Contains(message.Compaction.Summary, "ancient work") {
			t.Errorf("consumed old summary still on the wire: %q", message.Compaction.Summary)
		}
	}
	if len(tiers) != 2 || tiers[0] != 2 || tiers[1] != 1 {
		t.Fatalf("checkpoint tiers = %v, want [2 1] (merged old block then fresh fold)", tiers)
	}
	// The merged tier-2 block must sit where the consumed blocks were: first.
	if sess.Messages[0].Compaction == nil || tierOf(sess.Messages[0].Compaction) != 2 {
		t.Error("merged tier-2 block is not at the consumed range position")
	}
}

// The growth gate stops every-message folding: a checkpoint followed by a
// sliver of new conversation must not compact even when the size trigger is
// over the line from irreducible request costs.
func TestNeedsCompactionGatedByGrowth(t *testing.T) {
	c := &Compactor{Settings: DefaultSettings(), ContextLimit: 32000}
	sess := &sessionstore.Session{ID: "growth-test"}
	sess.Messages = append(sess.Messages, sessionstore.Message{
		Role: llm.RoleSystem, Content: "<conversation-checkpoint>",
		Compaction: &sessionstore.CompactionInfo{Summary: "prior work"},
	})
	sess.Messages = append(sess.Messages,
		sessionstore.Message{Role: llm.RoleUser, Content: "one more thing"},
		sessionstore.Message{Role: llm.RoleAssistant, Content: "done", Usage: llm.UsageInfo{InputTokens: 100000}},
	)
	if c.NeedsCompaction(sess) {
		t.Fatal("compaction fired on a sliver of new conversation; the growth gate must hold it")
	}
}

// Fifteen thousand genuinely new tokens after the checkpoint re-arms folding.
func TestNeedsCompactionRearmsAfterGrowth(t *testing.T) {
	c := &Compactor{Settings: DefaultSettings(), ContextLimit: 32000}
	sess := &sessionstore.Session{ID: "growth-test"}
	sess.Messages = append(sess.Messages, sessionstore.Message{
		Role: llm.RoleSystem, Content: "<conversation-checkpoint>",
		Compaction: &sessionstore.CompactionInfo{Summary: "prior work"},
	})
	sess.Messages = append(sess.Messages,
		sessionstore.Message{Role: llm.RoleUser, Content: strings.Repeat("new work paragraph. ", 4000)},
		sessionstore.Message{Role: llm.RoleAssistant, Content: "done", Usage: llm.UsageInfo{InputTokens: 100000}},
	)
	if !c.NeedsCompaction(sess) {
		t.Fatal("compaction stayed silent after 15k new tokens; the gate must re-arm")
	}
}

// Fold ids must be unique across Compactor instances: one instance is built
// per run, so a plain sequence restarts at fold-1 every run and each run's
// retention file overwrites the last.
func TestFoldIDsUniqueAcrossInstances(t *testing.T) {
	first := (&Compactor{}).recordFold(0, 5, 1)
	second := (&Compactor{}).recordFold(0, 5, 1)
	if first.ID == "" || second.ID == "" {
		t.Fatal("fold ids must not be empty")
	}
	if first.ID == second.ID {
		t.Fatalf("fold ids collided across instances: %q", first.ID)
	}
}
