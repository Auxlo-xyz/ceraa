package llm

import (
	"strings"
	"testing"
)

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

func TestEncodeMessagesIncludesToolResultName(t *testing.T) {
	messages := encodeMessages([]Item{{Kind: "text", Role: RoleTool, CallID: "call1", Name: "run", Output: "ok"}}, false)
	if len(messages) != 1 || messages[0]["name"] != "run" {
		t.Fatalf("encoded tool result = %#v", messages)
	}
}

func TestEncodeMessagesAddsGeminiThoughtSignatureOnlyWhenEnabled(t *testing.T) {
	item := Item{Kind: "tool_call", CallID: "call1", Name: "run", Args: `{"tool":"read","arguments":{}}`, ThoughtSignature: "sig"}
	enabled := encodeMessages([]Item{item}, true)
	disabled := encodeMessages([]Item{item}, false)
	enabledCalls := enabled[0]["tool_calls"].([]map[string]any)
	disabledCalls := disabled[0]["tool_calls"].([]map[string]any)
	if enabledCalls[0]["extra_content"] == nil {
		t.Fatal("Gemini thought signature was not serialized")
	}
	if _, ok := disabledCalls[0]["extra_content"]; ok {
		t.Fatal("thought signature was serialized for a non-Gemini provider")
	}
}

func TestReadStreamPreservesGeminiThoughtSignature(t *testing.T) {
	client := &StreamingClient{}
	var got ProviderResponse
	err := client.readStream(strings.NewReader("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call1\",\"extra_content\":{\"google\":{\"thought_signature\":\"sig\"}},\"function\":{\"name\":\"run\",\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n\n"), func(response ProviderResponse) {
		if len(response.ToolCalls) > 0 {
			got = response
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].ThoughtSignature != "sig" {
		t.Fatalf("thought signature = %+v", got.ToolCalls)
	}
}

func TestRepairToolArgumentsJSON(t *testing.T) {
	malformed := `{"arguments": {"command": "ls -la /dev}, "tool": "bash"}`
	repaired, changed := RepairToolArgumentsJSON(malformed)
	if !changed || repaired != `{"arguments": {"command": "ls -la /dev"}, "tool": "bash"}` {
		t.Fatalf("repaired=%q changed=%t", repaired, changed)
	}
	truncated := `{"path":"/index.html"`
	repaired, changed = RepairToolArgumentsJSON(truncated)
	if !changed || repaired != `{"path":"/index.html"}` {
		t.Fatalf("truncated repaired=%q changed=%t", repaired, changed)
	}
	if unchanged, changed := RepairToolArgumentsJSON(repaired); changed || unchanged != repaired {
		t.Fatalf("valid arguments changed: %q", unchanged)
	}
	if unchanged, changed := RepairToolArgumentsJSON(`{"arguments":`); changed || unchanged != `{"arguments":` {
		t.Fatalf("unrecoverable arguments changed: %q", unchanged)
	}
}

func TestParseTextToolCall(t *testing.T) {
	text := `<tool_call>run
<arg_key>arguments</arg_key>
<arg_value>{"command": "cat /app/package.json", "timeout": 10000}</arg_value><arg_key>tool</arg_key>
<arg_value>bash</arg_value>
</tool_call>`
	call, ok := ParseTextToolCall(text)
	if !ok || call.Name != "run" || !strings.Contains(call.Arguments, `"tool":"bash"`) {
		t.Fatalf("parsed text tool call = %#v, ok=%t", call, ok)
	}
	if !ContainsTextToolCall(text) {
		t.Fatal("text tool call was not detected")
	}
}

func TestExtractSingleArgValue(t *testing.T) {
	value, ok := ExtractSingleArgValue(`prefix <arg_value>final answer</arg_value> suffix`)
	if !ok || value != "final answer" {
		t.Fatalf("extracted=%q ok=%t", value, ok)
	}
	if _, ok := ExtractSingleArgValue(`<arg_value>one</arg_value><arg_value>two</arg_value>`); ok {
		t.Fatal("accepted multiple arg values")
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

func TestUsageInfoParsesProviderAliases(t *testing.T) {
	var usage UsageInfo
	err := usage.UnmarshalJSON([]byte(`{"prompt_tokens":120,"completion_tokens":8,"total_tokens":128,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 120 || usage.OutputTokens != 8 || usage.TotalTokens != 128 {
		t.Fatalf("unexpected usage totals: %+v", usage)
	}
	if usage.CachedInputTokens != 40 || usage.UncachedInputTokens() != 80 {
		t.Fatalf("unexpected cache usage: %+v", usage)
	}
	if usage.ReasoningTokens != 3 {
		t.Fatalf("unexpected reasoning usage: %+v", usage)
	}
}

func TestEstimateTokenBreakdown(t *testing.T) {
	tools := []ToolDef{{Name: "read", Description: "read a file", Parameters: map[string]any{"type": "object"}}}
	messages := []Item{
		{Kind: "text", Role: RoleSystem, Text: "system instructions"},
		{Kind: "text", Role: RoleUser, Text: "read the file"},
		{Kind: "text", Role: RoleTool, Output: "file contents"},
		{Kind: "reasoning", Reason: "reasoning"},
	}
	breakdown := EstimateTokenBreakdown(messages, tools)
	if breakdown.System == 0 || breakdown.Tools == 0 || breakdown.User == 0 || breakdown.ToolOutput == 0 || breakdown.Reasoning == 0 {
		t.Fatalf("missing source breakdown: %+v", breakdown)
	}
	if breakdown.Total() == 0 {
		t.Fatal("expected non-zero breakdown")
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
