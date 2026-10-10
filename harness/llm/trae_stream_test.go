package llm

import (
	"strings"
	"testing"
)

// Wire ids travel verbatim: the endpoint routes on config_name + model_name,
// so the picker offers ids that resolve to the exact pair.
func TestTraeWireModel(t *testing.T) {
	config, model := TraeWireModel("glm-5.1")
	if config != "glm-5.1" || model != "glm-5__v2" {
		t.Errorf("glm-5.1 -> %q %q", config, model)
	}
	config, model = TraeWireModel("trae/kimi-k2.6")
	if config != "kimi-k2.6" || model != "kimi-k2.6__v2" {
		t.Errorf("trae/kimi-k2.6 -> %q %q", config, model)
	}
	config, model = TraeWireModel("GLM-5.1")
	if config != "glm-5.1" || model != "glm-5__v2" {
		t.Errorf("case-insensitive GLM-5.1 -> %q %q", config, model)
	}
	config, model = TraeWireModel("some-future-model")
	if config != "some-future-model" || model != "some-future-model" {
		t.Errorf("unknown id should pass through, got %q %q", config, model)
	}
	if !IsTraeModel("deepseek-v4-pro") || !IsTraeModel("trae/minimax-m3") {
		t.Error("known ids not recognised")
	}
	if IsTraeModel("gpt-9") || IsTraeModel("") {
		t.Error("unknown or empty id accepted")
	}
	if DefaultTraeModel == "" {
		t.Error("no default model")
	}
}

func TestTraeMessagesRenderToolTrafficAsText(t *testing.T) {
	items := []Item{
		{Kind: "text", Role: RoleSystem, Text: "sys"},
		{Kind: "text", Role: RoleUser, Text: "hi"},
		{Kind: "tool_call", CallID: "c1", Name: "read", Args: `{"p":"x"}`, Text: "reading"},
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "read", Output: "file contents"},
		{Kind: "reasoning", Reason: "should be dropped"},
	}
	msgs := traeMessages(items)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (reasoning dropped)", len(msgs))
	}
	if msgs[0]["role"] != "system" {
		t.Errorf("first role = %v", msgs[0]["role"])
	}
	assistant, _ := msgs[2]["content"].([]map[string]any)
	if len(assistant) == 0 || !strings.Contains(assistant[0]["text"].(string), "[called read(") {
		t.Errorf("assistant call line = %v", msgs[2]["content"])
	}
	if msgs[3]["role"] != "user" {
		t.Errorf("tool result role = %v, want user", msgs[3]["role"])
	}
	tool, _ := msgs[3]["content"].([]map[string]any)
	if len(tool) == 0 || !strings.Contains(tool[0]["text"].(string), "file contents") {
		t.Errorf("tool result content = %v", msgs[3]["content"])
	}
}

// The stream contract: output frames emit text, tool_calls frames accumulate
// by id and emit whole, token_usage updates usage, error frames fail the
// turn, and the final delta carries the finish reason.
func TestTraeConsumeStream(t *testing.T) {
	sse := "event: output\n" +
		`data: {"response":"Hello"}` + "\n\n" +
		"event: output\n" +
		`data: {"tool_calls":[{"id":"call-1","function":{"name":"read","arguments":"{\"path\":\"x\"}"}}]}` + "\n\n" +
		"event: token_usage\n" +
		`data: {"prompt_tokens":100,"completion_tokens":20}` + "\n\n" +
		"data: [DONE]\n\n"
	var deltas []ProviderResponse
	err := traeConsumeStream(strings.NewReader(sse), func(d ProviderResponse) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	var text, finish string
	var calls []StreamedToolCall
	var usage *UsageInfo
	for _, d := range deltas {
		text += d.Content
		if len(d.ToolCalls) > 0 {
			calls = d.ToolCalls
		}
		if d.Usage != nil && d.Usage.InputTokens > 0 {
			usage = d.Usage
		}
		if d.Done {
			finish = d.FinishReason
		}
	}
	if text != "Hello" {
		t.Errorf("text = %q", text)
	}
	if len(calls) != 1 || calls[0].ID != "call-1" || calls[0].Name != "read" {
		t.Errorf("calls = %+v", calls)
	}
	if !strings.Contains(calls[0].Arguments, `"path"`) {
		t.Errorf("call args = %q", calls[0].Arguments)
	}
	if usage == nil || usage.InputTokens != 100 || usage.OutputTokens != 20 {
		t.Errorf("usage = %+v", usage)
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %q, want tool_calls", finish)
	}
}

func TestTraeConsumeStreamError(t *testing.T) {
	sse := "event: error\n" + `data: {"code":4001,"message":"bad model"}` + "\n\n"
	called := false
	err := traeConsumeStream(strings.NewReader(sse), func(ProviderResponse) { called = true })
	if err == nil || !strings.Contains(err.Error(), "4001") {
		t.Errorf("expected code 4001 error, got %v (deltas: %v)", err, called)
	}
}
