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
