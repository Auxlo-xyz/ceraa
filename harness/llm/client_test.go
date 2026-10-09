package llm

import (
	"context"
	"encoding/json"
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
