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

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
