package compaction

import (
	"strings"
	"testing"

	"sooraa/harness/llm"
	"sooraa/harness/sessionstore"
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
