package compaction

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

// rollSession puts n standing checkpoints ahead of a history long enough to
// fold, modeled on the stored-prefix fixture: a system prompt, then the chain,
// then two exchanges plus a repeat-heavy tool result for the budget to bite on.
func rollSession(n int) *sessionstore.Session {
	messages := []sessionstore.Message{{Role: llm.RoleSystem, Content: "you are the agent"}}
	for i := 0; i < n; i++ {
		messages = append(messages, sessionstore.Message{
			Role:       llm.RoleUser,
			Content:    "<conversation-checkpoint>",
			Compaction: &sessionstore.CompactionInfo{Summary: fmt.Sprintf("checkpoint %d: facts from fold %d", i, i), Seq: i},
		})
	}
	messages = append(messages,
		sessionstore.Message{Role: llm.RoleUser, Content: "fix the index"},
		sessionstore.Message{Role: llm.RoleAssistant, Content: "on it", ToolCalls: []sessionstore.ToolCallRef{{ID: "c1", Name: "read", Args: `{"path":"/repo/types.ts"}`}}},
		sessionstore.Message{Role: llm.RoleTool, ToolCallID: "c1", ToolName: "read", ToolResult: strings.Repeat("a matching line\n", 200)},
		sessionstore.Message{Role: llm.RoleUser, Content: "keep going"},
		sessionstore.Message{Role: llm.RoleAssistant, Content: "halfway there"},
		sessionstore.Message{Role: llm.RoleUser, Content: "and now the final exchange"},
		sessionstore.Message{Role: llm.RoleAssistant, Content: "the final exchange is done"},
	)
	return &sessionstore.Session{ID: "tg-roll", Messages: messages}
}

func standingCheckpointsIn(sess *sessionstore.Session) int {
	standing := 0
	for _, message := range sess.Messages {
		if message.Compaction != nil {
			standing++
		}
	}
	return standing
}

func TestARollIsOneRequest(t *testing.T) {
	srv := newSummaryServer(t, validTestSummary())
	defer srv.close()
	sess := rollSession(MaxCheckpointChain + 1)
	c := inBandCompactor(srv.server, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fix the index"}})
	ok, err := c.Compact(context.Background(), sess)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !ok {
		t.Fatal("nothing was compacted")
	}
	if len(srv.bodies) != 1 {
		t.Fatalf("the roll ran %d requests, want the fold's own single request", len(srv.bodies))
	}
	if standing := standingCheckpointsIn(sess); standing != 1 {
		t.Fatalf("%d checkpoints remain, want only the one the fold wrote", standing)
	}
	for _, message := range sess.Messages {
		if message.Compaction != nil {
			if err := validateSummary(message.Compaction.Summary); err != nil {
				t.Errorf("the stored summary fails validation: %v", err)
			}
		}
	}
}

func TestARollFailureLeavesTheChainStanding(t *testing.T) {
	srv := newSummaryServer(t, "Looks good, moving on.", "Looks good, moving on.")
	defer srv.close()
	sess := rollSession(MaxCheckpointChain + 1)
	before := len(sess.Messages)
	c := inBandCompactor(srv.server, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fix the index"}})
	if _, err := c.Compact(context.Background(), sess); err == nil {
		t.Fatal("a roll over two rejected summaries succeeded")
	}
	if standing := standingCheckpointsIn(sess); standing != MaxCheckpointChain+1 {
		t.Fatalf("%d checkpoints remain, want all %d", standing, MaxCheckpointChain+1)
	}
	if len(sess.Messages) != before {
		t.Fatalf("the failed roll rewrote the session: %d -> %d messages", before, len(sess.Messages))
	}
}

// TestARollRequestsARollSizedOutput pins the summary budget: a request whose
// reply dissolves the standing chain asks for SummaryRollOutputTokens, while a
// plain fold keeps the ordinary 4,096 ceiling. The first live roll truncated
// at 4,096 and dropped the template's final section, so this agreed budget is
// load-bearing.
func TestARollRequestsARollSizedOutput(t *testing.T) {
	live := []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fix the index"}}
	maxTokens := func(standing int) float64 {
		t.Helper()
		srv := newSummaryServer(t, validTestSummary())
		defer srv.close()
		sess := rollSession(standing)
		c := inBandCompactor(srv.server, live)
		if _, err := c.Compact(context.Background(), sess); err != nil {
			t.Fatalf("compact over %d checkpoints: %v", standing, err)
		}
		body := srv.lastBody(t)
		requested, ok := body["max_tokens"].(float64)
		if !ok {
			t.Fatalf("the request carries no max_tokens: %v", body)
		}
		return requested
	}
	if got := maxTokens(MaxCheckpointChain + 1); got != float64(SummaryRollOutputTokens) {
		t.Errorf("a roll requested %.0f tokens, want %d", got, SummaryRollOutputTokens)
	}
	if got := maxTokens(MaxCheckpointChain); got != float64(SummaryOutputTokens) {
		t.Errorf("a plain fold requested %.0f tokens, want %d", got, SummaryOutputTokens)
	}
}

// TestARollPreemptsTierMerges pins the precedence: a chain standing over the
// cap is consolidated by the fold's own request even when merge-eligible old
// blocks are inside it, and no separate distillation request runs on top.
func TestARollPreemptsTierMerges(t *testing.T) {
	srv := newSummaryServer(t, validTestSummary())
	defer srv.close()
	sess := rollSession(MaxCheckpointChain + 1)
	for i := 0; i < 3; i++ {
		sess.Messages[1+i].Compaction.Tier = 1
		sess.Messages[1+i].Compaction.Generation = "old"
	}
	c := inBandCompactor(srv.server, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fix the index"}})
	ok, err := c.Compact(context.Background(), sess)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !ok {
		t.Fatal("nothing was compacted")
	}
	if len(srv.bodies) != 1 {
		t.Fatalf("the roll ran %d requests over merge-eligible blocks, want only the fold's single request", len(srv.bodies))
	}
	if standing := standingCheckpointsIn(sess); standing != 1 {
		t.Fatalf("%d checkpoints remain, want only the one the fold wrote", standing)
	}
	for _, message := range sess.Messages {
		if message.Compaction != nil && message.Compaction.Tier != 1 {
			t.Errorf("the surviving checkpoint is tier %d, want the fold's tier 1", message.Compaction.Tier)
		}
	}
}

func TestConsolidationClauseIsRequestScoped(t *testing.T) {
	clause := consolidationClause()
	live := []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fix the index"}}
	lastInstruction := func(standing int) string {
		t.Helper()
		srv := newSummaryServer(t, validTestSummary())
		defer srv.close()
		sess := rollSession(standing)
		c := inBandCompactor(srv.server, live)
		ok, err := c.Compact(context.Background(), sess)
		if err != nil || !ok {
			t.Fatalf("compact over %d checkpoints: ok=%v err=%v", standing, ok, err)
		}
		body := srv.lastBody(t)
		messages, _ := body["messages"].([]any)
		if len(messages) == 0 {
			t.Fatal("the request carries no messages")
		}
		last, _ := messages[len(messages)-1].(map[string]any)
		text, _ := last["content"].(string)
		return text
	}
	if text := lastInstruction(MaxCheckpointChain); strings.Contains(text, clause) {
		t.Errorf("the clause fired at a chain the cap still admits: %.120q", text)
	}
	if text := lastInstruction(MaxCheckpointChain + 1); !strings.Contains(text, clause) {
		t.Error("the clause is missing from the instruction whose reply dissolves the chain")
	}
}
