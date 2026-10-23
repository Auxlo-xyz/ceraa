package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// These two tests used to assert that addPromptCacheFields wrote cache_control
// onto the request body. That is the bug: the field is only read on content
// blocks, so explicit mode was returning 200 and caching nothing, and the tests
// pinned the broken behaviour in place. The envelope must stay untouched and the
// marker must land on a block.
func TestExplicitModeLeavesTheEnvelopeAlone(t *testing.T) {
	body := map[string]any{}
	addPromptCacheFields(body, ProviderConfig{
		BaseURL:         "https://openrouter.ai/api/v1",
		PromptCacheMode: "explicit",
	}, StreamRequest{PromptCacheKey: "cache-key", SessionID: "session-id"})
	if _, present := body["cache_control"]; present {
		t.Fatalf("cache_control was written to the request envelope, where no API reads it: %#v", body["cache_control"])
	}
	if body["session_id"] != "session-id" {
		t.Errorf("session routing was lost: %#v", body)
	}
}

func TestTheDefaultTTLIsNotSent(t *testing.T) {
	system, seam := strings.Repeat("stable token ", 800), 0
	seam = len(system) + 2
	system += "\n\n<current-date>\nToday is 2026-09-27.\n</current-date>"
	msgs := []map[string]any{{"role": "system", "content": system}}
	applyCacheBreakpoints(msgs, ProviderConfig{PromptCacheMode: "explicit"}, seam)

	control := firstCacheControl(t, msgs)
	if control["type"] != "ephemeral" {
		t.Fatalf("cache control = %#v", control)
	}
	if _, present := control["ttl"]; present {
		t.Errorf("the 5m default should not send an explicit ttl: %#v", control)
	}
}

// A 1h write costs 2x base input against 1.25x for 5m, so it must be asked for
// rather than assumed.
func TestConfiguredTTLIsSentOnTheBlock(t *testing.T) {
	system := strings.Repeat("stable token ", 800) + "\n\n<current-date>\nToday is 2026-09-27.\n</current-date>"
	seam := len(strings.Repeat("stable token ", 800)) + 2
	msgs := []map[string]any{{"role": "system", "content": system}}
	applyCacheBreakpoints(msgs, ProviderConfig{PromptCacheMode: "explicit", PromptCacheTTL: "1h"}, seam)

	control := firstCacheControl(t, msgs)
	if control["type"] != "ephemeral" || control["ttl"] != "1h" {
		t.Fatalf("cache control = %#v", control)
	}
}

// An unsupported TTL is dropped back to the default rather than forwarded, since
// the API would reject it and a 400 costs the whole run.
func TestAnUnsupportedTTLFallsBackInsteadOfBeingForwarded(t *testing.T) {
	system := strings.Repeat("stable token ", 800) + "\n\n<current-date>\nToday is 2026-09-27.\n</current-date>"
	seam := len(strings.Repeat("stable token ", 800)) + 2
	msgs := []map[string]any{{"role": "system", "content": system}}
	applyCacheBreakpoints(msgs, ProviderConfig{PromptCacheMode: "explicit", PromptCacheTTL: "3h"}, seam)

	if control := firstCacheControl(t, msgs); control["ttl"] == "3h" {
		t.Errorf("an unsupported ttl was forwarded to the API: %#v", control)
	}
}

// firstCacheControl returns the marker on the first block that carries one.
func firstCacheControl(t *testing.T, msgs []map[string]any) map[string]any {
	t.Helper()
	for _, msg := range msgs {
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, raw := range blocks {
			if block, ok := raw.(map[string]any); ok {
				if control, ok := block["cache_control"].(map[string]any); ok {
					return control
				}
			}
		}
	}
	t.Fatalf("no cache_control on any content block: %#v", msgs)
	return nil
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

// The context a request occupied is the whole prompt plus the reply. On a
// split-reporting endpoint the cache read is reported beside the fresh input, so
// it must be added back; where a cache hit is already inside the input, adding it
// would count the same tokens twice.
func TestUsageContextTokens(t *testing.T) {
	cases := []struct {
		name  string
		usage UsageInfo
		want  int
	}{
		{"split reporting adds the cached prefix", UsageInfo{InputTokens: 1616, CachedInputTokens: 76416, OutputTokens: 2287, TotalTokens: 3903}, 1616 + 76416 + 2287},
		{"a subset cache hit is not counted twice", UsageInfo{InputTokens: 1000, CachedInputTokens: 900, OutputTokens: 50, TotalTokens: 1050}, 1050},
		{"a fully cached prompt still counts", UsageInfo{CachedInputTokens: 5000, OutputTokens: 40, TotalTokens: 40}, 5040},
		{"reasoning tokens stay counted", UsageInfo{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 8, TotalTokens: 128}, 128},
		{"nothing reported stays zero", UsageInfo{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.usage.ContextTokens(); got != tc.want {
				t.Errorf("ContextTokens(%+v) = %d, want %d", tc.usage, got, tc.want)
			}
		})
	}
}

func TestUsageCacheReadRatio(t *testing.T) {
	usage := UsageInfo{InputTokens: 100, CachedInputTokens: 75}
	if usage.CacheReadRatio() != 0.75 {
		t.Fatalf("cache read ratio = %v", usage.CacheReadRatio())
	}
}

// Split-reporting endpoints count only fresh tokens in input, so cached/input
// exceeds 1 and the old formula saturated at exactly 1 for every high-cache
// turn. The denominator must include the cached prefix in that case.
func TestUsageCacheReadRatioSplitReporting(t *testing.T) {
	usage := UsageInfo{InputTokens: 184, CachedInputTokens: 22_016}
	want := float64(22_016) / float64(184+22_016)
	if got := usage.CacheReadRatio(); got != want {
		t.Fatalf("split cache read ratio = %v, want %v", got, want)
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

// TestCompatibilityAdapterNeverInjectsSignature pins that the OpenAI-compatibility
// adapter replays only signatures the model actually returned.
//
// The adapter carries a signature in extra_content, a field outside the OpenAI
// schema, so there is no dependable way to know whether a fabricated bypass
// value would be honoured or rejected by a given shim. Inventing one is
// guesswork, so unsigned calls stay unsigned and Gemini is served by the native
// transport instead.
func TestCompatibilityAdapterNeverInjectsSignature(t *testing.T) {
	items := []Item{
		{Kind: "text", Role: RoleUser, Text: "look around"},
		{Kind: "tool_call", CallID: "c1", Name: "bash", Args: `{"command":"ls"}`, ThoughtSignature: "real-sig"},
		{Kind: "tool_call", CallID: "c2", Name: "run", Args: `{"command":"ls"}`},
		{Kind: "text", Role: RoleTool, CallID: "c2", Name: "run", Output: "out"},
	}
	msgs := encodeMessagesWithCompatibility(items, true, defaultCompatibility())
	got := toolCallSignatures(msgs)
	if got["c1"] != "real-sig" {
		t.Fatalf("signed call signature = %q, want it replayed unchanged", got["c1"])
	}
	if signature, present := got["c2"]; present {
		t.Fatalf("unsigned call must stay unsigned, got %q", signature)
	}
}

// TestToolResultNeverCarriesThoughtSignature pins Google's OpenAI request
// shape: a tool result is a plain functionResponse. Signature material belongs
// only inside assistant tool_calls[].
func TestToolResultNeverCarriesThoughtSignature(t *testing.T) {
	items := []Item{
		{Kind: "tool_call", CallID: "unsigned", Name: "ls", Args: "{}"},
		{Kind: "text", Role: RoleTool, CallID: "unsigned", Name: "ls", Output: "out"},
	}
	msgs := encodeMessagesWithCompatibility(items, true, defaultCompatibility())
	for _, msg := range msgs {
		if msg["role"] != string(RoleTool) {
			continue
		}
		if _, present := msg["extra_content"]; present {
			t.Fatalf("tool message must not carry extra_content: %v", msg)
		}
	}
}

func toolCallSignatures(msgs []map[string]any) map[string]string {
	got := map[string]string{}
	for _, msg := range msgs {
		calls, ok := msg["tool_calls"].([]map[string]any)
		if !ok {
			continue
		}
		for _, call := range calls {
			id, _ := call["id"].(string)
			extra, _ := call["extra_content"].(map[string]any)
			google, _ := extra["google"].(map[string]any)
			if signature, _ := google["thought_signature"].(string); signature != "" {
				got[id] = signature
			}
		}
	}
	return got
}

// TestSignatureStatsUnderpinsDiagnostic pins the counters behind the diagnostic
// line, so a failing run is readable instead of guesswork.
func TestSignatureStatsUnderpinsDiagnostic(t *testing.T) {
	stats := geminiSignatureStats([]Item{
		{Kind: "tool_call", CallID: "a", Name: "bash", ThoughtSignature: "sig"},
		{Kind: "tool_calls", ToolCalls: []ToolCall{
			{ID: "c", Name: "weather", ThoughtSignature: "sig"},
			{ID: "d", Name: "weather"},
		}},
	})
	if stats.signed != 2 || stats.unsigned != 1 {
		t.Fatalf("stats = %+v, want signed=2 unsigned=1", stats)
	}
}

// TestDescribeEncodedTurnRevealsSignaturePlacement pins the diagnostic that makes
// a Gemini 400 readable: it must show, per assistant turn, every function name
// and whether a signature rides along, while never leaking message content.
func TestDescribeEncodedTurnRevealsSignaturePlacement(t *testing.T) {
	items := []Item{
		{Kind: "text", Role: RoleUser, Text: "secret question"},
		{Kind: "tool_call", CallID: "c1", Name: "bash", Args: `{"command":"ls secret"}`, ThoughtSignature: "real-signature-value"},
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "bash", Output: "secret output"},
		{Kind: "tool_call", CallID: "c2", Name: "read", Args: "{}"},
	}
	msgs := encodeMessagesWithCompatibility(items, true, defaultCompatibility())
	got := describeEncodedTurn(msgs)
	for _, want := range []string{
		"user",
		"assistant/tool_calls(bash:sig20)",
		"tool",
		"assistant/tool_calls(read:none)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("turn %q missing %q", got, want)
		}
	}
	for _, leak := range []string{"secret question", "secret output", "ls secret"} {
		if strings.Contains(got, leak) {
			t.Fatalf("turn leaked message content %q: %s", leak, got)
		}
	}
}

// TestStreamFailsFastWithoutEndpoint pins that a missing base URL is reported as
// a configuration problem, not as "unsupported protocol scheme" after three
// pointless retries.
func TestStreamFailsFastWithoutEndpoint(t *testing.T) {
	client := New(ProviderConfig{Model: "gemini-3.5-flash-lite", MaxRetries: 3})
	err := client.Stream(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(ProviderResponse) {})
	if err == nil {
		t.Fatal("expected an error when no endpoint is configured")
	}
	if !strings.Contains(err.Error(), "no provider endpoint configured") {
		t.Fatalf("error = %v, want a configuration error naming the missing endpoint", err)
	}
	if strings.Contains(err.Error(), "unsupported protocol scheme") {
		t.Fatalf("error still leaks the transport symptom: %v", err)
	}
}

func TestRequestHostNamesEndpointOnly(t *testing.T) {
	if got := requestHost("https://generativelanguage.googleapis.com/v1beta"); got != "generativelanguage.googleapis.com" {
		t.Fatalf("requestHost = %q", got)
	}
}

// ExtraHeadersFunc exists because a correlation id in the static map would be
// computed once and reused for the life of the provider. This is the contract:
// it sees the session this request resolved to, it gets a fresh value every
// call, and it cannot take over the headers the transport owns.
func TestExtraHeadersFuncIsEvaluatedPerRequestAndCannotShadowTransportHeaders(t *testing.T) {
	var seen []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Snapshot the request headers by name, so the assertions are about what
		// actually left the process rather than about what the config held.
		snapshot := map[string]string{}
		for _, key := range []string{"X-Opencode-Request", "X-Opencode-Session", "X-Opencode-Client", "Authorization", "Content-Type", "Accept"} {
			snapshot[key] = r.Header.Get(key)
		}
		seen = append(seen, snapshot)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	calls := 0
	client := New(ProviderConfig{
		BaseURL: server.URL,
		Model:   "gpt-5",
		APIKey:  "test-key",
		ExtraHeaders: map[string]string{
			"x-opencode-client": "ceraa",
		},
		ExtraHeadersFunc: func(sessionID string) map[string]string {
			calls++
			return map[string]string{
				"x-opencode-session": sessionID,
				"x-opencode-request": "req-" + sessionID,
				// Deliberately hostile: a provider extra naming a transport
				// header must not win.
				"Authorization": "hijacked",
				"Content-Type":  "text/plain",
			}
		},
	})

	for i := 0; i < 2; i++ {
		err := client.Stream(t.Context(), StreamRequest{
			Messages:  []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
			MaxTokens: 8,
			SessionID: "ses-abc",
		}, func(ProviderResponse) {})
		if err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
	}

	if calls != 2 {
		t.Fatalf("ExtraHeadersFunc called %d times for 2 requests, want once each", calls)
	}
	for i, snapshot := range seen {
		if snapshot["Authorization"] != "Bearer test-key" {
			t.Errorf("request %d: Authorization = %q, a provider extra replaced the credential", i, snapshot["Authorization"])
		}
		if snapshot["X-Opencode-Client"] != "ceraa" {
			t.Errorf("request %d: x-opencode-client = %q, the static map was not applied", i, snapshot["X-Opencode-Client"])
		}
		if !strings.HasPrefix(snapshot["Content-Type"], "application/json") {
			t.Errorf("request %d: Content-Type = %q, a provider extra replaced it", i, snapshot["Content-Type"])
		}
		if snapshot["Accept"] != "text/event-stream" {
			t.Errorf("request %d: Accept = %q, a provider extra replaced it", i, snapshot["Accept"])
		}
		if snapshot["X-Opencode-Session"] != "ses-abc" {
			t.Errorf("request %d: x-opencode-session = %q, want the session this request resolved to", i, snapshot["X-Opencode-Session"])
		}
	}
	if seen[0]["X-Opencode-Request"] == "" {
		t.Error("x-opencode-request was not sent")
	}
}

// A provider refusing the reasoning level is the one 400 worth asking again
// about, and the retry goes out without the level.
func TestReasoningRetryFiresOnAParameterRejection(t *testing.T) {
	var attempts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "reasoning_effort") {
			attempts = append(attempts, "with-reasoning")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"reasoning_effort max is not supported for this model"}}`)
			return
		}
		attempts = append(attempts, "without-reasoning")
		w.Header().Set("Content-Type", "text/event-stream")
		encoded, _ := json.Marshal("ok")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", encoded)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := New(ProviderConfig{BaseURL: server.URL, APIKey: "k", Model: "m", MaxRetries: 0})
	var seen strings.Builder
	err := client.Stream(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
		Reason:   ReasonMax,
	}, func(d ProviderResponse) { seen.WriteString(d.Content) })
	if err != nil {
		t.Fatalf("the retry did not recover: %v", err)
	}
	if len(attempts) != 2 || attempts[0] != "with-reasoning" || attempts[1] != "without-reasoning" {
		t.Fatalf("attempts = %v, want one with and one without reasoning", attempts)
	}
	if seen.String() != "ok" {
		t.Errorf("content = %q", seen.String())
	}
}

// A 400 that is not about reasoning is a genuinely bad request. Retrying it
// only repeats the failure and doubles the bill.
func TestReasoningRetryStaysOffForOtherBadRequests(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"messages: text content blocks must be non-empty"}}`)
	}))
	defer server.Close()
	client := New(ProviderConfig{BaseURL: server.URL, APIKey: "k", Model: "m", MaxRetries: 0})
	err := client.Stream(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
		Reason:   ReasonHigh,
	}, func(ProviderResponse) {})
	if err == nil {
		t.Fatal("a bad request succeeded")
	}
	if attempts != 1 {
		t.Fatalf("sent %d requests, want 1", attempts)
	}
}

// The Codex transport sends no reasoning, so attributing any to a request says
// a fifth of its cost is somewhere it is not.
func TestBreakdownAttributesReasoningOnlyWhereItIsSent(t *testing.T) {
	messages := []Item{
		{Kind: "text", Role: RoleSystem, Text: "sys"},
		{Kind: "text", Role: RoleAssistant, Text: "answer", Reason: strings.Repeat("r", 400)},
	}
	replayed := EstimateTokenBreakdown(messages, nil)
	dropped := EstimateTokenBreakdownFor(messages, nil, false)
	if replayed.Reasoning == 0 {
		t.Fatal("the default breakdown stopped counting reasoning")
	}
	if dropped.Reasoning != 0 {
		t.Fatalf("a transport that sends no reasoning was charged %d tokens for it", dropped.Reasoning)
	}
	if dropped.Total() <= 0 || replayed.Total() <= dropped.Total() {
		t.Fatalf("totals: replayed %d, dropped %d", replayed.Total(), dropped.Total())
	}
	// The turn still counts, under the source that does reach the provider.
	if dropped.Conversation == 0 {
		t.Fatal("the assistant turn stopped being attributed to anything")
	}
}

// A provider error can arrive as an SSE data event inside a 200 response, and
// the Cline gateway does exactly that: {"error":{...}} then [DONE]. The reader
// used to see no choices, treat [DONE] as a clean end, and report an empty
// success, which surfaced to the user as "The model completed without
// producing a final response." while the real cause was a provider-side 400.
func TestReadStreamSurfacesProviderErrorInsideSuccessStream(t *testing.T) {
	client := &StreamingClient{}
	body := "data: {\"error\":{\"code\":\"stream_initialization_failed\",\"message\":\"Failed to create stream: request failed with status 400: invalid_request_error at messages.57.reasoning_details.0\",\"type\":\"stream_error\"}}\n\n" +
		"data: [DONE]\n\n"
	err := client.readStream(strings.NewReader(body), func(ProviderResponse) {})
	if err == nil {
		t.Fatal("expected the provider error, got a clean end of stream")
	}
	if !strings.Contains(err.Error(), "invalid_request_error") || !strings.Contains(err.Error(), "reasoning_details.0") {
		t.Fatalf("error = %q, want the provider's own message", err)
	}
	var streamErr *StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error type = %T, want *StreamError", err)
	}
	if streamErr.Retryable {
		t.Fatal("a malformed request must not be retried")
	}

	// A null error is what every ordinary chunk carries and must not fail.
	ok := "data: {\"error\":null,\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	if err := client.readStream(strings.NewReader(ok), func(ProviderResponse) {}); err != nil {
		t.Fatalf("null error field failed the stream: %v", err)
	}
}

// The gateway's message is a chain of hops and then the provider's own error as
// embedded JSON. Everything before the last layer is transport, and the version
// that kept it pushed the actual reason past a fold in every place it was shown.
func TestProviderStreamErrorKeepsTheInnermostReason(t *testing.T) {
	body := `data: {"error":{"code":"stream_initialization_failed","message":"Failed to create stream: inference request failed: failed to generate stream from Vercel: failed to invoke model minimax-m3.1-flash: request failed with status 400: {\"error\":{\"message\":\"invalid params, messages must not be empty (2013)\",\"type\":\"bad_request_error\",\"param\":{\"error\":\"invalid params\"}}}","type":"stream_error"}}
data: [DONE]

`
	client := &StreamingClient{}
	err := client.readStream(strings.NewReader(body), func(ProviderResponse) {})
	if err == nil {
		t.Fatal("expected the provider error, got a clean end of stream")
	}
	want := "invalid params, messages must not be empty (2013)"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want %q", err, want)
	}
	if strings.Contains(err.Error(), "Failed to create stream") || strings.Contains(err.Error(), "Vercel") {
		t.Fatalf("error = %q, still carries the gateway's wrapper chain", err)
	}
	if len([]rune(err.Error())) > 260 {
		t.Fatalf("error is %d runes, want a single short line", len([]rune(err.Error())))
	}
}

func TestProviderStreamErrorClassifiesTransientFailures(t *testing.T) {
	for _, tc := range []struct {
		detail string
		want   bool
	}{
		{"stream_error: Rate limit exceeded", true},
		{"upstream returned 503 Service Unavailable", true},
		// Cline's opaque sentence is a gateway failure, not a request the
		// provider refused to parse. The live audit died on it unretried.
		{"Provider returned error", true},
		{"invalid_request_error: expected object, received array", false},
	} {
		if got := retryableProviderError(tc.detail); got != tc.want {
			t.Fatalf("retryableProviderError(%q) = %v, want %v", tc.detail, got, tc.want)
		}
	}
	if _, failed := providerStreamError(nil, "", "error"); !failed {
		t.Fatal("type=error with no detail should still fail the stream")
	}
}

// The generic sentence hides the upstream reason in metadata.raw when the
// gateway has one, and the reason is what decides whether a retry can help.
func TestAGenericGatewayErrorKeepsTheUpstreamDetail(t *testing.T) {
	raw := json.RawMessage(`{"message":"Provider returned error","code":500,"metadata":{"raw":"upstream: 503 service unavailable"}}`)
	detail, failed := providerStreamError(raw, "", "")
	if !failed {
		t.Fatal("the generic body was read as a clean stream")
	}
	if !strings.Contains(detail, "503 service unavailable") {
		t.Errorf("detail = %q, want the metadata reason", detail)
	}
	if !retryableProviderError(detail) {
		t.Error("the upstream detail was not classified for a retry")
	}
	// A numeric code must not cost the message, which a string-typed field would.
	raw = json.RawMessage(`{"message":"invalid params","code":400}`)
	if detail, failed := providerStreamError(raw, "", ""); !failed || detail != "invalid params" {
		t.Errorf("detail = %q failed=%v, want the message with a numeric code", detail, failed)
	}
}

// A model that requires the echo rejects the whole history when one assistant
// step has no reasoning, so every assistant message carries a non-empty value.
func TestReasoningEchoFillsEveryAssistantMessage(t *testing.T) {
	items := []Item{
		{Kind: "tool_calls", ToolCalls: []ToolCall{{ID: "c1", Name: "read", Args: "{}"}}},
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "read", Output: "ok"},
		{Kind: "text", Role: RoleAssistant, Text: "done"},
		{Kind: "text", Role: RoleAssistant, Text: "reasoned", Reason: "because"},
		{Kind: "text", Role: RoleUser, Text: "next"},
	}
	msgs := encodeMessagesWithCompatibility(items, false, Compatibility{
		ReasoningField:         "reasoning_content",
		RequiresToolResultName: true,
		RequiresReasoningEcho:  true,
	})
	var assistants int
	for _, msg := range msgs {
		if msg["role"] != "assistant" {
			continue
		}
		assistants++
		text, _ := msg["reasoning_content"].(string)
		if text == "" {
			t.Errorf("assistant message has no reasoning_content: %#v", msg)
		}
	}
	if assistants != 3 {
		t.Fatalf("encoded %d assistant messages, want 3", assistants)
	}
	// Without the requirement the placeholder must not appear: it is a repair
	// for a demanded field, not a field every provider should receive.
	plain := encodeMessagesWithCompatibility(items, false, Compatibility{ReasoningField: "reasoning_content"})
	for _, msg := range plain {
		if msg["role"] == "assistant" && msg["reasoning_content"] == reasoningEchoPlaceholder {
			t.Errorf("the placeholder was sent without the requirement: %#v", msg)
		}
	}
}

// The recovery path has to work for a model Ceraa does not know in advance.
// The first request is refused over the missing echo, the retry carries it, and
// the second request must not repeat the mistake.
func TestMissingReasoningEchoIsRepairedOnceAndRetried(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests++
		messages, _ := body["messages"].([]any)
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			_, _ = w.Write([]byte(`data: {"error":{"message":"Missing required parameter: 'messages[2].reasoning_content'. This model needs the reasoning of every earlier assistant message"}}`))
			_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
			return
		}
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			if message["role"] != "assistant" {
				continue
			}
			text, _ := message["reasoning_content"].(string)
			if text == "" {
				t.Errorf("the retry still lacks reasoning_content: %#v", message)
			}
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client := New(ProviderConfig{BaseURL: server.URL, Model: "unknown-model", APIKey: "test", MaxRetries: 2})
	err := client.Stream(t.Context(), StreamRequest{
		Messages: []Item{
			{Kind: "text", Role: RoleUser, Text: "go"},
			{Kind: "text", Role: RoleAssistant, Text: "done"},
			{Kind: "text", Role: RoleUser, Text: "continue"},
		},
	}, func(ProviderResponse) {})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2: one refused and one repaired", requests)
	}
}
