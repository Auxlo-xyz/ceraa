package llm

import "testing"

func TestReasonLevelValid(t *testing.T) {
	tests := []struct {
		level ReasonLevel
		valid bool
	}{
		{ReasonLow, true},
		{ReasonMedium, true},
		{ReasonHigh, true},
		{ReasonXHigh, true},
		{ReasonMax, true},
		{"invalid", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := tt.level.Valid(); got != tt.valid {
			t.Errorf("ReasonLevel(%q).Valid() = %v, want %v", tt.level, got, tt.valid)
		}
	}
}

func TestItemString(t *testing.T) {
	item := Item{Kind: "text", Role: RoleSystem, Text: "hello"}
	if item.String() != "system: hello" {
		t.Errorf("expected 'system: hello', got %q", item.String())
	}

	tc := Item{Kind: "tool_call", Name: "bash", Args: "{\"cmd\":\"ls\"}", CallID: "123"}
	if !contains(tc.String(), "bash") {
		t.Errorf("expected tool call string to contain name, got %q", tc.String())
	}
}

func TestEnsureToolCallIDs(t *testing.T) {
	calls := EnsureToolCallIDs([]StreamedToolCall{{}, {ID: "same"}, {ID: "same"}}, "call_1")
	if calls[0].ID != "call_1_0" || calls[1].ID != "same" || calls[2].ID != "call_1_2" {
		t.Fatalf("unexpected IDs: %+v", calls)
	}
}

func TestMergeStreamedToolCalls(t *testing.T) {
	calls := MergeStreamedToolCalls(nil, []StreamedToolCall{
		{Index: 0, ID: "a", Name: "read", Arguments: `{"path":`},
		{Index: 1, ID: "b", Name: "glob", Arguments: `{"pattern":`},
	})
	calls = MergeStreamedToolCalls(calls, []StreamedToolCall{
		{Index: 0, ID: "a", Name: "read", Arguments: `{"path":"go.mod"}`},
		{Index: 1, ID: "b", Name: "glob", Arguments: `{"pattern":"*.go"}`},
	})
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(calls))
	}
	if calls[0].ID != "a" || calls[0].Arguments != `{"path":"go.mod"}` {
		t.Fatalf("unexpected first call: %+v", calls[0])
	}
	if calls[1].ID != "b" || calls[1].Arguments != `{"pattern":"*.go"}` {
		t.Fatalf("unexpected second call: %+v", calls[1])
	}
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
