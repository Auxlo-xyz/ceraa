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
	head, recent := selectMessageUnits(messages, 130, true)
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

func TestCollapseChainLeavesShortChainAlone(t *testing.T) {
	// The cap is the whole trade: while the chain is short it is free, because
	// nothing ahead of the newest checkpoint has moved. Rolling early would pay
	// a prefix rewrite for nothing.
	c := &Compactor{Settings: DefaultSettings(), ContextLimit: 32000}
	sess := &sessionstore.Session{ID: "s"}
	for i := 0; i < MaxCheckpointChain; i++ {
		sess.Messages = append(sess.Messages, sessionstore.Message{
			Role:    llm.RoleSystem,
			Content: "<conversation-checkpoint>",
			Compaction: &sessionstore.CompactionInfo{
				Summary: "summary",
				Seq:     i,
				Tier:    0,
			},
		})
	}
	before := len(sess.Messages)
	rewrites := sess.PrefixRewrites
	if c.collapseChain(t.Context(), sess) {
		t.Fatal("a chain at the cap was collapsed")
	}
	if len(sess.Messages) != before {
		t.Fatalf("messages changed: %d -> %d", before, len(sess.Messages))
	}
	if sess.PrefixRewrites != rewrites {
		t.Fatal("a no-op collapse counted a prefix rewrite")
	}
}

// A fold must never leave a tool result whose call was folded away.
//
// This is the shape that made the provider refuse the request outright:
// "tool_use_id found in tool_result blocks". The cut is computed from message
// counts, so a parallel batch whose results straddle it loses its calls to the
// summary and keeps its results on the wire.
func TestFoldNeverLeavesOrphanToolResults(t *testing.T) {
	srv := newSummaryServer(t, validTestSummary())
	defer srv.close()
	c := inBandCompactor(srv.server, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fold"}})
	c.Settings.Keep.Tokens = 100
	// Ten exchanges, each an assistant turn with a call and one result. Ten
	// keeps the age guard folding the oldest sixty percent, which is where a
	// count-based cut lands mid-exchange.
	messages := []sessionstore.Message{{Role: llm.RoleSystem, Content: "system"}}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("call-%02d", i)
		messages = append(messages,
			sessionstore.Message{Role: llm.RoleUser, Content: fmt.Sprintf("task-%02d", i)},
			sessionstore.Message{Role: llm.RoleAssistant, Content: "working", ToolCalls: []sessionstore.ToolCallRef{{ID: id, Name: "read", Args: `{}`}}},
			sessionstore.Message{Role: llm.RoleTool, ToolCallID: id, ToolName: "read", ToolResult: strings.Repeat("result body\n", 40)},
		)
	}
	sess := &sessionstore.Session{ID: "orphan-test", Messages: messages}
	if _, err := c.Compact(context.Background(), sess); err != nil {
		t.Fatalf("compact: %v", err)
	}
	for _, message := range sess.Messages {
		if message.Role != llm.RoleTool || message.ToolCallID == "" {
			continue
		}
		for _, other := range sess.Messages {
			if other.Role == llm.RoleAssistant {
				for _, call := range other.AllToolCalls() {
					if call.ID == message.ToolCallID {
						goto paired
					}
				}
			}
		}
		t.Errorf("tool result %q survived with no tool_use declaring it", message.ToolCallID)
	paired:
	}
}

// The sweep repairs a session that is already broken on disk, which a corrected
// cut cannot: the bad pair is stored, not being cut. A session written by an
// older build has to survive the next fold rather than stay refused forever.
func TestDropOrphanToolResultsRepairsStoredDamage(t *testing.T) {
	srv := newSummaryServer(t, validTestSummary())
	defer srv.close()
	c := inBandCompactor(srv.server, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fold"}})
	c.Settings.Keep.Tokens = 100
	messages := []sessionstore.Message{{Role: llm.RoleSystem, Content: "system"}}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("call-%02d", i)
		messages = append(messages,
			sessionstore.Message{Role: llm.RoleUser, Content: fmt.Sprintf("task-%02d", i)},
			sessionstore.Message{Role: llm.RoleAssistant, Content: "working", ToolCalls: []sessionstore.ToolCallRef{{ID: id, Name: "read", Args: `{}`}}},
			sessionstore.Message{Role: llm.RoleTool, ToolCallID: id, ToolName: "read", ToolResult: strings.Repeat("result body\n", 40)},
		)
	}
	// The damage: a result whose call was dropped by an earlier build.
	messages = append(messages, sessionstore.Message{Role: llm.RoleTool, ToolCallID: "call-dangling", ToolName: "read", ToolResult: "orphan"})
	sess := &sessionstore.Session{ID: "repair-test", Messages: messages}
	if _, err := c.Compact(context.Background(), sess); err != nil {
		t.Fatalf("compact: %v", err)
	}
	for _, message := range sess.Messages {
		if message.ToolCallID == "call-dangling" {
			t.Fatal("the orphan result survived the fold")
		}
	}
}

// A merge only removes checkpoint markers, but a checkpoint can stand between a
// turn and its results once an earlier fold cut through one. The sweep runs
// there too, for the same reason.
func TestMergeCheckpointsDropsOrphanToolResults(t *testing.T) {
	srv := newSummaryServer(t, validTestSummary())
	defer srv.close()
	c := inBandCompactor(srv.server, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "merge"}})
	sess := &sessionstore.Session{ID: "merge-orphan", Messages: []sessionstore.Message{
		{Role: llm.RoleSystem, Content: "system", Compaction: &sessionstore.CompactionInfo{Summary: "one"}},
		{Role: llm.RoleSystem, Content: "system", Compaction: &sessionstore.CompactionInfo{Summary: "two"}},
		{Role: llm.RoleTool, ToolCallID: "call-gone", ToolName: "read", ToolResult: "orphan"},
		{Role: llm.RoleUser, Content: "continue"},
	}}
	c.mergeCheckpoints(t.Context(), sess, []int{0, 1}, 2)
	for _, message := range sess.Messages {
		if message.ToolCallID == "call-gone" {
			t.Fatal("the orphan result survived the merge")
		}
	}
}

func TestMergeCheckpointsWithNoIndicesIsNoOp(t *testing.T) {
	c := &Compactor{Settings: DefaultSettings(), ContextLimit: 32000}
	sess := &sessionstore.Session{ID: "s", Messages: []sessionstore.Message{{Role: llm.RoleUser, Content: "hi"}}}
	c.mergeCheckpoints(t.Context(), sess, nil, 1)
	if len(sess.Messages) != 1 {
		t.Fatalf("an empty merge rewrote the session: %d messages", len(sess.Messages))
	}
	if sess.PrefixRewrites != 0 {
		t.Fatal("an empty merge counted a prefix rewrite")
	}
}

// The Codex transport sends no reasoning, so a fold trigger measured with it
// fires on less real history than the gate was written for.
func TestGrowthGateIsMeasuredAgainstWhatTheTransportSends(t *testing.T) {
	build := func() *sessionstore.Session {
		return &sessionstore.Session{Messages: []sessionstore.Message{
			{Compaction: &sessionstore.CompactionInfo{Summary: "earlier"}},
			{Role: llm.RoleUser, Content: "question"},
			{Role: llm.RoleAssistant, Content: "answer", Reasoning: strings.Repeat("r", 40000)},
		}}
	}
	c := New(llm.ProviderConfig{})
	withIt := growthSinceCheckpoint(build(), true)
	withoutIt := growthSinceCheckpoint(build(), false)
	if withIt <= withoutIt {
		t.Fatalf("growth counted with reasoning %d, without %d", withIt, withoutIt)
	}
	// Reasoning alone must not be able to open the gate.
	sess := &sessionstore.Session{Messages: []sessionstore.Message{
		{Compaction: &sessionstore.CompactionInfo{Summary: "earlier"}},
		{Role: llm.RoleAssistant, Content: "short", Reasoning: strings.Repeat("r", 200000)},
	}}
	if growthSinceCheckpoint(sess, false) > MinFoldGrowthTokens {
		t.Fatalf("unsent reasoning alone passed the %d-token growth gate", MinFoldGrowthTokens)
	}
	_ = c
}

// The keep budget decides how much recent history survives un-folded. Counting
// reasoning there shrinks it, so the fold eats history the model still needed.
func TestKeepBudgetHoldsMoreHistoryWhenReasoningIsNotReplayed(t *testing.T) {
	var messages []sessionstore.Message
	for i := 0; i < 12; i++ {
		messages = append(messages,
			sessionstore.Message{Role: llm.RoleUser, Content: fmt.Sprintf("q%d", i)},
			sessionstore.Message{Role: llm.RoleAssistant, Content: fmt.Sprintf("a%d", i), Reasoning: strings.Repeat("r", 2000)},
		)
	}
	// A keep budget that two exchanges of reasoning alone exceed, and that two
	// exchanges of answer text sit well inside. Counting the reasoning evicts one
	// of them; excluding it keeps both.
	withIt := recentExchangeCount(messages, 500, true)
	withoutIt := recentExchangeCount(messages, 500, false)
	if withoutIt <= withIt {
		t.Fatalf("kept %d exchanges counting reasoning, %d excluding it", withIt, withoutIt)
	}
}

func recentExchangeCount(messages []sessionstore.Message, keep int, replayed bool) int {
	_, recent := selectMessageUnits(messages, keep, replayed)
	return len(recent)
}

// A compactor nobody configured must keep measuring everything.
func TestReplaysReasoningDefaultsToTrue(t *testing.T) {
	if !New(llm.ProviderConfig{}).replaysReasoning() {
		t.Fatal("an unconfigured compactor stopped counting reasoning")
	}
	// And the transport decides it when the owner does not.
	c := New(llm.ProviderConfig{BaseURL: "https://chatgpt.com/backend-api/codex"})
	if c.replaysReasoning() {
		t.Fatal("a Codex compactor still counts reasoning")
	}
}
