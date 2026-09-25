package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamUsesOpenAICompatibilityAndReplaysReasoning(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("accept header = %q", r.Header.Get("Accept"))
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := New(ProviderConfig{
		BaseURL: server.URL,
		Model:   "gpt-5",
		APIKey:  "test",
		Compatibility: &Compatibility{
			MaxTokensField:         "max_completion_tokens",
			SupportsUsageStreaming: true,
			SupportsFinishReason:   true,
			SupportsDeveloperRole:  true,
			ReasoningField:         "reasoning_content",
		},
	})
	var responses []ProviderResponse
	err := client.Stream(t.Context(), StreamRequest{
		Messages:  []Item{{Kind: "reasoning", Reason: "prior reasoning"}, {Kind: "text", Role: RoleUser, Text: "continue"}},
		MaxTokens: 32,
	}, func(response ProviderResponse) { responses = append(responses, response) })
	if err != nil {
		t.Fatal(err)
	}
	if body["max_completion_tokens"] != float64(32) {
		t.Fatalf("body = %#v", body)
	}
	messages, _ := body["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	first, _ := messages[0].(map[string]any)
	if first["reasoning_content"] != "prior reasoning" {
		t.Fatalf("reasoning replay = %#v", first)
	}
	if len(responses) == 0 || !responses[len(responses)-1].Done || responses[len(responses)-1].FinishReason != "stop" {
		t.Fatalf("responses = %#v", responses)
	}
}

func TestAddPromptCacheFieldsUsesProviderDefaults(t *testing.T) {
	body := map[string]any{}
	sessionID := addPromptCacheFields(body, ProviderConfig{
		BaseURL:         "https://openrouter.ai/api/v1",
		PromptCacheMode: "auto",
	}, StreamRequest{PromptCacheKey: "cache-key", SessionID: "session-id"})
	if body["prompt_cache_key"] != "cache-key" || body["session_id"] != "session-id" || sessionID != "session-id" {
		t.Fatalf("cache fields = %#v, session = %q", body, sessionID)
	}
	if _, exists := body["cache_control"]; exists {
		t.Fatalf("auto mode added cache_control: %#v", body)
	}
}

func TestAddPromptCacheFieldsUsesOptionalTTL(t *testing.T) {
	body := map[string]any{}
	addPromptCacheFields(body, ProviderConfig{
		BaseURL:         "https://openrouter.ai/api/v1",
		PromptCacheMode: "explicit",
	}, StreamRequest{PromptCacheKey: "cache-key", SessionID: "session-id"})
	control, ok := body["cache_control"].(map[string]any)
	if !ok || control["type"] != "ephemeral" {
		t.Fatalf("cache control = %#v", body["cache_control"])
	}
	if _, exists := control["ttl"]; exists {
		t.Fatalf("default cache control forced a TTL: %#v", control)
	}
}

func TestAddPromptCacheFieldsUsesConfiguredTTL(t *testing.T) {
	body := map[string]any{}
	addPromptCacheFields(body, ProviderConfig{
		BaseURL:         "https://openrouter.ai/api/v1",
		PromptCacheMode: "explicit",
		PromptCacheTTL:  "3h",
	}, StreamRequest{PromptCacheKey: "cache-key", SessionID: "session-id"})
	control, ok := body["cache_control"].(map[string]any)
	if !ok || control["type"] != "ephemeral" || control["ttl"] != "3h" {
		t.Fatalf("cache control = %#v", body["cache_control"])
	}
}

func TestAddPromptCacheFieldsCanDisableCaching(t *testing.T) {
	body := map[string]any{}
	if sessionID := addPromptCacheFields(body, ProviderConfig{
		BaseURL:         "https://openrouter.ai/api/v1",
		PromptCacheMode: "off",
	}, StreamRequest{PromptCacheKey: "cache-key", SessionID: "session-id"}); sessionID != "" {
		t.Fatalf("disabled cache session = %q, body = %#v", sessionID, body)
	}
	if len(body) != 0 {
		t.Fatalf("disabled cache fields = %#v", body)
	}
}

func TestUsageCacheReadRatio(t *testing.T) {
	usage := UsageInfo{InputTokens: 100, CachedInputTokens: 75}
	if usage.CacheReadRatio() != 0.75 {
		t.Fatalf("cache read ratio = %v", usage.CacheReadRatio())
	}
}

func TestEncodeMessagesAttachesReasoningToAssistantText(t *testing.T) {
	messages := encodeMessages([]Item{{Kind: "text", Role: RoleAssistant, Text: "answer", Reason: "thinking", ReasoningDetails: json.RawMessage(`{"type":"summary"}`)}}, false)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	message := messages[0]
	if message["content"] != "answer" || message["reasoning_content"] != "thinking" {
		t.Fatalf("assistant message = %#v", message)
	}
	if message["reasoning_details"] == nil {
		t.Fatalf("reasoning details missing: %#v", message)
	}
}

func TestEncodeMessagesProvidesContentForEmptyToolResult(t *testing.T) {
	messages := encodeMessages([]Item{{Kind: "text", Role: RoleTool, CallID: "call1", Name: "read", Output: ""}}, false)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0]["content"] != "No output was produced." {
		t.Fatalf("empty tool result = %#v", messages[0])
	}
}

func TestEncodeMessagesSkipsEmptyReasoningOnlyAssistant(t *testing.T) {
	messages := encodeMessages([]Item{{Kind: "text", Role: RoleAssistant, Reason: "thinking"}}, false)
	if len(messages) != 0 {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestReadStreamRejectsMalformedOrUnterminatedData(t *testing.T) {
	client := &StreamingClient{}
	var got ProviderResponse
	if err := client.readStream(strings.NewReader("data: {bad}\n\n"), func(response ProviderResponse) { got = response }); err == nil {
		t.Fatal("expected malformed SSE error")
	}
	if err := client.readStream(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"), func(response ProviderResponse) { got = response }); err == nil {
		t.Fatal("expected missing terminal error")
	}
	if got.Content != "partial" {
		t.Fatalf("unexpected response: %#v", got)
	}
}

// TestToolResultNameIsAlwaysSent pins the fix for a provider that rejects a
// tool result with no name. Gemini translates a tool message into a
// function_response part and fails the whole request with
// "function_response.name: Name cannot be empty", so an omitted name is not a
// cosmetic difference: it costs the entire run.
func TestToolResultNameIsAlwaysSent(t *testing.T) {
	compatibility := defaultCompatibility()
	items := []Item{
		{Kind: "text", Role: RoleUser, Text: "run it"},
		{Kind: "tool_calls", ToolCalls: []ToolCall{{ID: "call-1", Name: "bash", Args: "{}"}}},
		// The stored result lost its name; it must be recovered from the call.
		{Kind: "text", Role: RoleTool, CallID: "call-1", Output: "done"},
		// Neither the item nor a matching call knows the name.
		{Kind: "text", Role: RoleTool, CallID: "call-2", Output: "done"},
		{Kind: "text", Role: RoleTool, CallID: "###", Output: "done"},
	}
	msgs := encodeMessagesWithCompatibility(items, false, compatibility)
	var toolNames []string
	for _, msg := range msgs {
		if msg["role"] == string(RoleTool) {
			name, _ := msg["name"].(string)
			toolNames = append(toolNames, name)
		}
	}
	if len(toolNames) != 3 {
		t.Fatalf("encoded %d tool messages, want 3", len(toolNames))
	}
	if toolNames[0] != "bash" {
		t.Fatalf("name recovered from the call = %q, want bash", toolNames[0])
	}
	for i, name := range toolNames {
		if strings.TrimSpace(name) == "" {
			t.Fatalf("tool message %d has an empty name: %q", i, name)
		}
	}
	// Distinct calls must not collapse to the same placeholder.
	if toolNames[1] == toolNames[2] {
		t.Fatalf("distinct calls produced the same fallback name %q", toolNames[1])
	}
}

// TestToolResultNameOmittedWhenNotRequired pins that a provider which does not
// need the field still gets no name, so this does not change other providers.
func TestToolResultNameOmittedWhenNotRequired(t *testing.T) {
	compatibility := defaultCompatibility()
	compatibility.RequiresToolResultName = false
	msgs := encodeMessagesWithCompatibility([]Item{
		{Kind: "text", Role: RoleTool, CallID: "call-1", Name: "bash", Output: "ok"},
	}, false, compatibility)
	if _, present := msgs[0]["name"]; present {
		t.Fatalf("name should be omitted when not required: %v", msgs[0])
	}
}

// TestFallbackToolResultName pins the placeholder shape: non-empty, and limited
// to characters providers accept in a function name.
func TestFallbackToolResultName(t *testing.T) {
	if got := fallbackToolResultName(""); got != "tool" {
		t.Fatalf("empty call id = %q, want tool", got)
	}
	if got := fallbackToolResultName("###"); got != "tool" {
		t.Fatalf("no usable characters = %q, want tool", got)
	}
	got := fallbackToolResultName("call-abc_123")
	if got != "tool_callabc_123" {
		t.Fatalf("got %q, want the call id with unsafe characters dropped", got)
	}
	long := strings.Repeat("a", 200)
	if len(fallbackToolResultName(long)) > 45 {
		t.Fatal("fallback name must be bounded")
	}
}

// TestUsesGeminiThoughtSignatures pins the detection rule. It must follow the
// model and host, never the profile slot: keying it on the provider id disabled
// the signature round-trip for env-configured Gemini models.
func TestUsesGeminiThoughtSignatures(t *testing.T) {
	cases := []struct {
		model, base string
		want        bool
	}{
		{"gemini-3.5-flash", "https://generativelanguage.googleapis.com/v1beta/openai", true},
		{"gemini-2.5-pro", "https://proxy.internal/v1", true},
		{"GEMINI-3.5-FLASH", "https://proxy.internal/v1", true},
		{"gemma-3-27b", "https://proxy.internal/v1", true},
		{"  gemini-3.5-flash  ", "", true},
		// Renamed behind a proxy, but Gemini's host: still needed.
		{"internal-coding-model", "https://generativelanguage.googleapis.com/v1beta/openai", true},
		{"gpt-5", "https://api.openai.com/v1", false},
		{"poolside/laguna-xs-2.1", "https://inference.poolside.ai/v1", false},
		{"", "", false},
	}
	for _, tc := range cases {
		if got := UsesGeminiThoughtSignatures(tc.model, tc.base); got != tc.want {
			t.Errorf("UsesGeminiThoughtSignatures(%q, %q) = %v, want %v", tc.model, tc.base, got, tc.want)
		}
	}
}

// TestThoughtSignatureIsReplayedOnToolCalls pins the round-trip Gemini 3
// requires: the signature the model returned on a function call must go back out
// on the next request, or the provider rejects the whole conversation.
func TestThoughtSignatureIsReplayedOnToolCalls(t *testing.T) {
	items := []Item{
		{Kind: "text", Role: RoleUser, Text: "list files"},
		{Kind: "tool_call", CallID: "c1", Name: "bash", Args: `{"command":"ls"}`, ThoughtSignature: "sig-abc"},
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "bash", Output: "go.mod"},
	}
	msgs := encodeMessagesWithCompatibility(items, true, defaultCompatibility())
	found := false
	for _, msg := range msgs {
		calls, ok := msg["tool_calls"].([]map[string]any)
		if !ok {
			continue
		}
		for _, call := range calls {
			extra, _ := call["extra_content"].(map[string]any)
			google, _ := extra["google"].(map[string]any)
			if google["thought_signature"] == "sig-abc" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("thought_signature was not replayed on the tool call: %v", msgs)
	}
	// With the flag off, no signature is sent, so other providers are unchanged.
	msgs = encodeMessagesWithCompatibility(items, false, defaultCompatibility())
	for _, msg := range msgs {
		if calls, ok := msg["tool_calls"].([]map[string]any); ok {
			for _, call := range calls {
				if _, present := call["extra_content"]; present {
					t.Fatal("extra_content must be absent when signatures are disabled")
				}
			}
		}
	}
}

// TestUnsignedToolCallGetsSkipSentinel pins the fix for a permanent 400.
//
// Gemini 3 validates the first functionCall part of every step in the current
// turn and rejects the whole request when it carries no thought_signature. A
// call recorded by an earlier provider can never obtain a real signature, so
// replaying that history fails on every retry. Google's documented escape hatch
// is the skip_thought_signature_validator sentinel.
func TestUnsignedToolCallGetsSkipSentinel(t *testing.T) {
	items := []Item{
		{Kind: "text", Role: RoleUser, Text: "look around"},
		// Signed by Gemini: the real signature must be replayed verbatim.
		{Kind: "tool_call", CallID: "c1", Name: "bash", Args: `{"command":"ls"}`, ThoughtSignature: "real-sig"},
		// Recorded by another provider: no signature exists and none can.
		{Kind: "tool_call", CallID: "chatcmpl-tool-abc", Name: "run", Args: `{"command":"ls"}`},
		{Kind: "text", Role: RoleTool, CallID: "chatcmpl-tool-abc", Name: "run", Output: "out"},
	}
	msgs := encodeMessagesWithCompatibility(items, true, defaultCompatibility())
	got := map[string]string{}
	for _, msg := range msgs {
		calls, ok := msg["tool_calls"].([]map[string]any)
		if !ok {
			continue
		}
		for _, call := range calls {
			extra, _ := call["extra_content"].(map[string]any)
			google, _ := extra["google"].(map[string]any)
			sig, _ := google["thought_signature"].(string)
			fn, _ := call["function"].(map[string]any)
			name, _ := fn["name"].(string)
			got[name] = sig
		}
	}
	if got["bash"] != "real-sig" {
		t.Fatalf("signed call signature = %q, want it replayed unchanged", got["bash"])
	}
	if got["run"] != skipThoughtSignatureValidator {
		t.Fatalf("unsigned call signature = %q, want %q", got["run"], skipThoughtSignatureValidator)
	}
}

// TestSkipSentinelOnlyOnFunctionCalls pins that the sentinel never leaks onto
// text or reasoning parts, which Gemini validates leniently.
func TestSkipSentinelOnlyOnFunctionCalls(t *testing.T) {
	items := []Item{
		{Kind: "text", Role: RoleAssistant, Text: "thinking out loud", Reason: ""},
		{Kind: "reasoning", Reason: "some reasoning"},
	}
	msgs := encodeMessagesWithCompatibility(items, true, defaultCompatibility())
	for _, msg := range msgs {
		if _, present := msg["extra_content"]; present {
			t.Fatalf("extra_content must not appear on a non-functionCall part: %v", msg)
		}
	}
}

// TestNoSentinelWhenSignaturesDisabled pins that other providers are untouched:
// no signature, no sentinel.
func TestNoSentinelWhenSignaturesDisabled(t *testing.T) {
	items := []Item{{Kind: "tool_call", CallID: "c1", Name: "bash", Args: "{}"}}
	msgs := encodeMessagesWithCompatibility(items, false, defaultCompatibility())
	calls, _ := msgs[0]["tool_calls"].([]map[string]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v", msgs[0]["tool_calls"])
	}
	if _, present := calls[0]["extra_content"]; present {
		t.Fatal("extra_content must be absent when signatures are disabled")
	}
}
