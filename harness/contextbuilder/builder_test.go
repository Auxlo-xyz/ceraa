package contextbuilder

import (
	"strings"
	"testing"

	"sooraa/harness/llm"
	"sooraa/harness/sessionstore"
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
