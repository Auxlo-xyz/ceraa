package contextbuilder

import (
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

func TestNewBuilder(t *testing.T) {
	sess := &sessionstore.Session{ID: "test"}
	b := New(sess, "system prompt")
	if b == nil {
		t.Fatal("expected non-nil builder")
	}
}

func TestBuildIncludesSystemPrompt(t *testing.T) {
	sess := &sessionstore.Session{ID: "test"}
	b := New(sess, "you are a helpful assistant")
	req := b.Build()
	if len(req.Messages) == 0 {
		t.Fatal("expected messages")
	}
	if req.Messages[0].Kind != "text" || req.Messages[0].Role != llm.RoleSystem {
		t.Fatalf("expected first message to be system, got %v", req.Messages[0])
	}
	if !strings.Contains(req.Messages[0].Text, "helpful") {
		t.Fatalf("expected system prompt in first message, got %q", req.Messages[0].Text)
	}
}

func TestBuildIncludesConversation(t *testing.T) {
	sess := &sessionstore.Session{
		ID: "test",
		Messages: []sessionstore.Message{
			{Role: llm.RoleUser, Content: "hello"},
		},
	}
	b := New(sess, "system prompt")
	req := b.Build()
	// system + user = 2 minimum
	if len(req.Messages) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(req.Messages))
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != llm.RoleUser {
		t.Fatalf("expected last message to be user, got %v", last.Role)
	}
	if last.Text != "hello" {
		t.Fatalf("expected 'hello', got %q", last.Text)
	}
}

func TestBuildAttachesReasoningToAssistantMessage(t *testing.T) {
	sess := &sessionstore.Session{Messages: []sessionstore.Message{
		{Role: llm.RoleAssistant, Content: "The answer is ready.", Reasoning: "I checked the files."},
	}}
	messages := New(sess, "system").Build().Messages
	found := false
	for _, item := range messages {
		if item.Kind == "reasoning" {
			t.Fatalf("reasoning was emitted as a standalone item: %+v", messages)
		}
		if item.Role == llm.RoleAssistant && item.Text == "The answer is ready." {
			found = true
			if item.Reason != "I checked the files." {
				t.Fatalf("reasoning was not attached: %+v", item)
			}
		}
	}
	if !found {
		t.Fatalf("assistant message missing: %+v", messages)
	}
}

func TestBuildHandlesToolCalls(t *testing.T) {
	sess := &sessionstore.Session{
		ID: "test",
		Messages: []sessionstore.Message{
			{
				Role:     llm.RoleAssistant,
				ToolCall: &sessionstore.ToolCallRef{ID: "call_1", Name: "bash", Args: "{\"command\":\"ls\"}"},
			},
			{
				Role:       llm.RoleTool,
				ToolCallID: "call_1",
				ToolResult: "file1.txt\nfile2.txt",
			},
		},
	}
	b := New(sess, "system")
	req := b.Build()
	// system + assistant tool_call + tool result = 3
	if len(req.Messages) < 3 {
		t.Fatalf("expected at least 3 messages, got %d", len(req.Messages))
	}
	// Check tool call item
	foundCall := false
	foundResult := false
	for _, m := range req.Messages {
		if m.Kind == "tool_call" && m.Name == "bash" {
			foundCall = true
		}
		if m.Role == llm.RoleTool && m.Output == "file1.txt\nfile2.txt" {
			foundResult = true
			if m.Name != "bash" {
				t.Fatalf("tool result name = %q", m.Name)
			}
		}
	}
	if !foundCall {
		t.Fatal("expected tool_call message")
	}
	if !foundResult {
		t.Fatal("expected tool result message")
	}
}

func TestBuildPreservesEmptyToolResultWithPresenceMarker(t *testing.T) {
	sess := &sessionstore.Session{Messages: []sessionstore.Message{
		{Role: llm.RoleAssistant, ToolCalls: []sessionstore.ToolCallRef{{ID: "call_empty", Name: "bash", Args: `{"command":"mkdir -p /tmp/example"}`}}},
		{Role: llm.RoleTool, ToolCallID: "call_empty", ToolName: "run", ToolResultPresent: true},
	}}
	req := New(sess, "system").Build()
	if len(req.Messages) < 3 {
		t.Fatalf("expected tool pair, got %d messages", len(req.Messages))
	}
	result := req.Messages[len(req.Messages)-1]
	if result.Role != llm.RoleTool || result.CallID != "call_empty" || result.Output != "" {
		t.Fatalf("empty tool result = %+v", result)
	}
}

func TestBuildWithLimitKeepsCompleteToolTurn(t *testing.T) {
	sess := &sessionstore.Session{Messages: []sessionstore.Message{
		{Role: llm.RoleUser, Content: strings.Repeat("old ", 100)},
		{Role: llm.RoleAssistant, ToolCalls: []sessionstore.ToolCallRef{{ID: "old_call", Name: "read", Args: `{"path":"old"}`}}},
		{Role: llm.RoleTool, ToolCallID: "old_call", ToolResult: "old result"},
		{Role: llm.RoleUser, Content: "latest"},
		{Role: llm.RoleAssistant, ToolCalls: []sessionstore.ToolCallRef{{ID: "new_call", Name: "read", Args: `{"path":"new"}`}}},
		{Role: llm.RoleTool, ToolCallID: "new_call", ToolResult: "new result"},
	}}
	req := New(sess, "system").BuildWithLimit(80)
	if EstimateMessages(req.Messages) > 80 {
		t.Fatalf("request exceeds limit: %d", EstimateMessages(req.Messages))
	}
	foundCall := false
	for _, item := range req.Messages {
		if item.Kind == "tool_call" && item.CallID == "new_call" {
			foundCall = true
		}
		if item.Kind == "tool_call" && item.CallID == "old_call" {
			t.Fatal("old incomplete turn was retained")
		}
	}
	if !foundCall {
		t.Fatal("latest tool call was dropped")
	}
}

func TestTruncatePreservesHeadAndTail(t *testing.T) {
	long := strings.Repeat("A", 200)
	result := Truncate(long, 30)
	// Truncate adds a marker in the middle, so result can exceed 30 chars
	if len(result) > 50 {
		t.Fatalf("expected truncated output around 30 chars, got %d", len(result))
	}
	if !strings.Contains(result, "truncated") {
		t.Fatal("expected truncation marker")
	}
}

func TestEstimateTokens(t *testing.T) {
	// 4 chars per token
	if EstimateTokens("hello world") != 2 {
		t.Fatalf("expected 2 tokens for 11 chars, got %d", EstimateTokens("hello world"))
	}
}

func TestFormatMessagesIntoPrompt(t *testing.T) {
	items := []llm.Item{
		{Kind: "text", Role: llm.RoleSystem, Text: "sys"},
		{Kind: "text", Role: llm.RoleUser, Text: "hello"},
	}
	result := FormatMessagesIntoPrompt(items)
	if !strings.Contains(result, "SYSTEM:") {
		t.Fatal("expected SYSTEM in output")
	}
	if !strings.Contains(result, "USER:") {
		t.Fatal("expected USER in output")
	}
}

// A result behind the immediate exchange travels as a pointer: its head, so
// the shape of it survives, plus the marker naming where the full text lives.
// The immediate exchange itself renders whole.
func TestStaleToolResultsRenderAsPointers(t *testing.T) {
	spooled := "Large tool output saved to /sessions/tool-output/run_op.txt (48210 bytes). It is greppable from bash.\nShape: 47.1 kB, 900 lines\nHead:\n" + strings.Repeat("old line\n", 200)
	sess := &sessionstore.Session{
		ID: "test",
		Messages: []sessionstore.Message{
			{Role: llm.RoleUser, Content: "read the log"},
			{Role: llm.RoleAssistant, Content: "reading", ToolCalls: []sessionstore.ToolCallRef{{ID: "c1", Name: "read", Args: `{}`}}},
			{Role: llm.RoleTool, ToolCallID: "c1", ToolName: "read", ToolResult: spooled, ToolResultPresent: true},
			// The fold that ends the epoch. Everything behind this point has
			// been summarised, so its bodies are worth shipping as pointers;
			// everything after it has not, and travels whole.
			{Role: llm.RoleSystem, Content: "<conversation-checkpoint>", Compaction: &sessionstore.CompactionInfo{Summary: "the log was read"}},
			{Role: llm.RoleUser, Content: "and now this"},
			{Role: llm.RoleAssistant, Content: "on it", ToolCalls: []sessionstore.ToolCallRef{{ID: "c2", Name: "bash", Args: `{}`}}},
			{Role: llm.RoleTool, ToolCallID: "c2", ToolName: "bash", ToolResult: "fresh output", ToolResultPresent: true},
		},
	}
	stored := sess.Messages[2].ToolResult
	req := New(sess, "system prompt").Build()
	var stale, fresh string
	for _, item := range req.Messages {
		if item.Role != llm.RoleTool {
			continue
		}
		if strings.Contains(item.Output, "fresh output") {
			fresh = item.Output
		} else {
			stale = item.Output
		}
	}
	if fresh != "fresh output" {
		t.Errorf("the immediate result was not rendered whole: %q", fresh)
	}
	if !strings.Contains(stale, "/sessions/tool-output/run_op.txt") {
		t.Errorf("the stale pointer lost the spool path: %.160q", stale)
	}
	if got := strings.Count(stale, "old line"); got >= 200 {
		t.Errorf("the stale body travelled whole: all 200 lines present")
	}
	if len(stale) >= len(spooled) {
		t.Errorf("nothing was saved: %d chars in, %d out", len(spooled), len(stale))
	}
	if !strings.Contains(stale, "older output") {
		t.Errorf("the stale pointer names no recovery: %.160q", stale)
	}
	if sess.Messages[2].ToolResult != stored {
		t.Error("rendering mutated the stored message")
	}
}

// Small stale results travel whole: pointerizing them would cost a marker to
// save nothing.
func TestSmallStaleResultsTravelWhole(t *testing.T) {
	sess := &sessionstore.Session{
		ID: "test",
		Messages: []sessionstore.Message{
			{Role: llm.RoleUser, Content: "first"},
			{Role: llm.RoleAssistant, Content: "ok"},
			{Role: llm.RoleTool, ToolCallID: "c1", ToolName: "read", ToolResult: "tiny", ToolResultPresent: true},
			{Role: llm.RoleUser, Content: "second"},
		},
	}
	req := New(sess, "system prompt").Build()
	for _, item := range req.Messages {
		if item.Role == llm.RoleTool && item.Output != "tiny" {
			t.Errorf("a small stale result was rewritten: %q", item.Output)
		}
	}
}

func TestPointerizeEpochBoundaryIsZeroWithoutCheckpoint(t *testing.T) {
	messages := []sessionstore.Message{
		{Role: llm.RoleUser, Content: "hello"},
		{Role: llm.RoleAssistant, Content: "hi"},
		{Role: llm.RoleTool, ToolResult: strings.Repeat("x", staleResultChars*2)},
	}
	if got := pointerizeEpochBoundary(messages); got != 0 {
		t.Fatalf("boundary = %d on a session with no checkpoint, want 0", got)
	}
	if out := pointerizeStaleResults(messages); out[2].ToolResult != messages[2].ToolResult {
		t.Fatal("a result ahead of an absent checkpoint was rewritten")
	}
}

func TestPointerizeEpochBoundarySitsAfterNewestCheckpoint(t *testing.T) {
	behind := strings.Repeat("y", staleResultChars*2)
	inside := strings.Repeat("z", staleResultChars*2)
	messages := []sessionstore.Message{
		{Role: llm.RoleUser, Content: "old"},
		{Role: llm.RoleTool, ToolResult: behind},
		{Role: llm.RoleSystem, Content: "<conversation-checkpoint>", Compaction: &sessionstore.CompactionInfo{Summary: "s1"}},
		{Role: llm.RoleUser, Content: "recent"},
		{Role: llm.RoleTool, ToolResult: inside},
	}
	if got := pointerizeEpochBoundary(messages); got != 3 {
		t.Fatalf("boundary = %d, want 3 (one past the checkpoint)", got)
	}
	out := pointerizeStaleResults(messages)
	if len(out) != len(messages) {
		t.Fatalf("length changed: %d", len(out))
	}
	if out[1].ToolResult == behind {
		t.Fatal("a result behind the boundary was left whole")
	}
	if out[4].ToolResult != inside {
		t.Fatal("a result inside the epoch was rewritten")
	}
	if messages[1].ToolResult != behind || messages[4].ToolResult != inside {
		t.Fatal("the stored messages were mutated")
	}
}

func TestPointerizeIsStableAcrossTurns(t *testing.T) {
	// The boundary must not follow the conversation. Appending a turn has to
	// leave every already-sent byte alone, or the provider's prefix cache stops
	// matching from the flip point on every run start.
	body := strings.Repeat("z", staleResultChars*2)
	messages := []sessionstore.Message{
		{Role: llm.RoleUser, Content: "old"},
		{Role: llm.RoleTool, ToolResult: body},
		{Role: llm.RoleSystem, Content: "<conversation-checkpoint>", Compaction: &sessionstore.CompactionInfo{Summary: "s1"}},
	}
	first := pointerizeStaleResults(messages)
	appended := append(append([]sessionstore.Message(nil), messages...),
		sessionstore.Message{Role: llm.RoleUser, Content: "new turn"})
	second := pointerizeStaleResults(appended)
	if first[1].ToolResult == body {
		t.Fatal("the result behind the boundary was not pointerized")
	}
	if first[1].ToolResult != second[1].ToolResult {
		t.Fatal("an already-sent result flipped between turns")
	}
	if second[3].Content != "new turn" {
		t.Fatal("the appended turn did not survive")
	}
}
