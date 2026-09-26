package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUsesGeminiNative pins transport selection. Native is the default for
// Google's host, and an explicitly configured OpenAI-compatibility URL is
// honoured as an opt-in to the adapter.
func TestUsesGeminiNative(t *testing.T) {
	cases := []struct {
		model, base string
		want        bool
	}{
		{"gemini-3-pro-preview", "https://generativelanguage.googleapis.com/v1beta", true},
		{"gemini-2.5-flash", "https://generativelanguage.googleapis.com/v1beta/", true},
		{"gemma-3-27b", "https://generativelanguage.googleapis.com/v1beta", true},
		// Explicit compatibility opt-out.
		{"gemini-3-pro-preview", "https://generativelanguage.googleapis.com/v1beta/openai", false},
		{"gemini-3-pro-preview", "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", false},
		// Not Google's host, so a Gemini-named model is someone else's proxy.
		{"gemini-3-pro-preview", "https://api.openrouter.ai/api/v1", false},
		{"gemini-3-pro-preview", "https://api.atria-asi.ai/v1", false},
		{"gpt-5", "https://api.openai.com/v1", false},
		{"", "", false},
	}
	for _, tc := range cases {
		if got := UsesGeminiNative(tc.model, tc.base); got != tc.want {
			t.Errorf("UsesGeminiNative(%q, %q) = %v, want %v", tc.model, tc.base, got, tc.want)
		}
	}
}

// TestFromProviderSelectsNativeTransport pins that every call site gets
// transport selection for free from the shared constructor.
func TestFromProviderSelectsNativeTransport(t *testing.T) {
	native := FromProvider("https://generativelanguage.googleapis.com/v1beta", "key", "gemini-3-pro-preview")
	if !native.GeminiNative {
		t.Fatal("Gemini on Google's host must use the native transport")
	}
	compat := FromProvider("https://api.openai.com/v1", "key", "gpt-5")
	if compat.GeminiNative {
		t.Fatal("a non-Gemini provider must not use the native transport")
	}
}

func TestGeminiStreamURL(t *testing.T) {
	cases := []struct{ base, model, want string }{
		{
			"https://generativelanguage.googleapis.com/v1beta", "gemini-3-pro-preview",
			"https://generativelanguage.googleapis.com/v1beta/models/gemini-3-pro-preview:streamGenerateContent?alt=sse",
		},
		{
			"https://generativelanguage.googleapis.com/v1beta/", "gemini-3-pro-preview",
			"https://generativelanguage.googleapis.com/v1beta/models/gemini-3-pro-preview:streamGenerateContent?alt=sse",
		},
		{
			"https://generativelanguage.googleapis.com/v1beta/models/gemini-3-pro-preview:streamGenerateContent", "gemini-3-pro-preview",
			"https://generativelanguage.googleapis.com/v1beta/models/gemini-3-pro-preview:streamGenerateContent",
		},
	}
	for _, tc := range cases {
		if got := geminiStreamURL(tc.base, tc.model); got != tc.want {
			t.Errorf("geminiStreamURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

// TestEncodeGeminiContentsSeparatesSystemInstruction pins that the system prompt
// travels as systemInstruction rather than as a contents turn.
func TestEncodeGeminiContentsSeparatesSystemInstruction(t *testing.T) {
	contents, system := encodeGeminiContents([]Item{
		{Kind: "text", Role: RoleSystem, Text: "You are Ceraa."},
		{Kind: "text", Role: RoleUser, Text: "hello"},
	})
	if system == nil || len(system.Parts) != 1 || system.Parts[0].Text != "You are Ceraa." {
		t.Fatalf("systemInstruction = %#v", system)
	}
	if len(contents) != 1 || contents[0].Role != "user" || contents[0].Parts[0].Text != "hello" {
		t.Fatalf("contents = %#v", contents)
	}
}

// TestEncodeGeminiMergesFunctionResponses pins the rule that cost us a 400 on the
// compatibility layer: all function responses for one step belong to a single
// user turn. Sending one turn per result is the interleaving Google rejects.
func TestEncodeGeminiMergesFunctionResponses(t *testing.T) {
	contents, _ := encodeGeminiContents([]Item{
		{Kind: "text", Role: RoleUser, Text: "compare"},
		{Kind: "tool_calls", ToolCalls: []ToolCall{
			{ID: "c1", Name: "weather", Args: `{"city":"Paris"}`, ThoughtSignature: "sig-a"},
			{ID: "c2", Name: "weather", Args: `{"city":"London"}`},
		}},
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "weather", Output: "cold"},
		{Kind: "text", Role: RoleTool, CallID: "c2", Name: "weather", Output: "mild"},
	})
	if len(contents) != 3 {
		t.Fatalf("expected user, model, user turns, got %d: %#v", len(contents), contents)
	}
	responses := contents[2]
	if responses.Role != "user" || len(responses.Parts) != 2 {
		t.Fatalf("function responses must share one user turn: %#v", responses)
	}
	for _, part := range responses.Parts {
		if part.FunctionResponse == nil {
			t.Fatalf("expected a functionResponse part, got %#v", part)
		}
	}
}

// TestEncodeGeminiReplaysSignatureAndBypassesSyntheticCall pins the two cases
// that matter: a signature the model returned is replayed verbatim, and a call
// with no possible signature gets Google's documented bypass value instead.
func TestEncodeGeminiReplaysSignatureAndBypassesSyntheticCall(t *testing.T) {
	contents, _ := encodeGeminiContents([]Item{
		{Kind: "tool_calls", ToolCalls: []ToolCall{
			{ID: "c1", Name: "bash", Args: `{"command":"ls"}`, ThoughtSignature: "real-sig"},
		}},
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "bash", Output: "go.mod"},
		{Kind: "tool_calls", ToolCalls: []ToolCall{
			{ID: "c2", Name: "ls", Args: `{"path":"."}`},
		}},
	})
	signed := contents[0].Parts[0]
	if signed.ThoughtSignature != "real-sig" {
		t.Fatalf("real signature = %q, want it replayed unchanged", signed.ThoughtSignature)
	}
	synthetic := contents[2].Parts[0]
	if synthetic.ThoughtSignature != skipThoughtSignatureValidator {
		t.Fatalf("unsigned call = %q, want %q", synthetic.ThoughtSignature, skipThoughtSignatureValidator)
	}
}

// TestEncodeGeminiParallelBatchKeepsOnlyFirstSignature pins Google's documented
// parallel shape: the first functionCall carries the signature, the rest stay
// unsigned, even when the whole batch is synthetic.
func TestEncodeGeminiParallelBatchKeepsOnlyFirstSignature(t *testing.T) {
	calls := []ToolCall{
		{ID: "c1", Name: "weather", Args: `{"city":"Paris"}`},
		{ID: "c2", Name: "weather", Args: `{"city":"London"}`},
		{ID: "c3", Name: "weather", Args: `{"city":"Rome"}`},
	}
	parts := geminiFunctionCallParts(calls, "")
	if len(parts) != 3 {
		t.Fatalf("parts = %#v", parts)
	}
	if parts[0].ThoughtSignature != skipThoughtSignatureValidator {
		t.Fatalf("first call = %q, want the bypass value", parts[0].ThoughtSignature)
	}
	for _, part := range parts[1:] {
		if part.ThoughtSignature != "" {
			t.Fatalf("subsequent parallel call must stay unsigned, got %q", part.ThoughtSignature)
		}
	}
}

// TestEncodeGeminiReplaysGenuineParallelBatchVerbatim pins that a real signature
// is never overwritten by a bypass value, and that later calls in a genuine
// batch keep their original empty state.
func TestEncodeGeminiReplaysGenuineParallelBatchVerbatim(t *testing.T) {
	parts := geminiFunctionCallParts([]ToolCall{
		{ID: "c1", Name: "weather", Args: `{}`, ThoughtSignature: "real-sig"},
		{ID: "c2", Name: "weather", Args: `{}`},
	}, "")
	if parts[0].ThoughtSignature != "real-sig" {
		t.Fatalf("first call = %q, want real-sig", parts[0].ThoughtSignature)
	}
	if parts[1].ThoughtSignature != "" {
		t.Fatalf("second parallel call = %q, want it unsigned", parts[1].ThoughtSignature)
	}
}

// TestEncodeGeminiDecodesToolArguments pins that arguments are sent as a decoded
// object, which is what a native functionCall requires, and that unparseable
// arguments degrade to an empty object rather than failing the request.
func TestEncodeGeminiDecodesToolArguments(t *testing.T) {
	parts := geminiFunctionCallParts([]ToolCall{{ID: "c1", Name: "bash", Args: `{"command":"ls -la"}`}}, "")
	if parts[0].FunctionCall.Args["command"] != "ls -la" {
		t.Fatalf("args = %#v", parts[0].FunctionCall.Args)
	}
	broken := geminiFunctionCallParts([]ToolCall{{ID: "c1", Name: "bash", Args: `{"command":`}}, "")
	if len(broken[0].FunctionCall.Args) != 0 {
		t.Fatalf("unparseable args = %#v, want empty", broken[0].FunctionCall.Args)
	}
}

// TestGeminiResponsePayload pins that a tool result becomes the JSON object a
// function response requires, while an already structured result keeps its shape.
func TestGeminiResponsePayload(t *testing.T) {
	structured := geminiResponsePayload(`{"status":"delayed"}`)
	if structured["status"] != "delayed" {
		t.Fatalf("structured payload = %#v", structured)
	}
	plain := geminiResponsePayload("plain text output")
	if plain["output"] != "plain text output" {
		t.Fatalf("plain payload = %#v", plain)
	}
	empty := geminiResponsePayload("")
	if empty["output"] == "" {
		t.Fatal("an empty result must still carry a non-empty payload")
	}
}

// TestSanitizeGeminiSchemaDropsUnsupportedKeywords pins that keywords Gemini's
// declaration schema rejects are removed rather than failing the whole request.
func TestSanitizeGeminiSchemaDropsUnsupportedKeywords(t *testing.T) {
	cleaned := sanitizeGeminiSchema(map[string]any{
		"type":    "object",
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "path",
				"pattern":     "^/",
				"$comment":    "drop me",
			},
			"paths": map[string]any{
				"type":     "array",
				"items":    map[string]any{"type": "string", "uniqueItems": true},
				"$comment": "drop me too",
			},
		},
		"required": []any{"path"},
	})
	if _, present := cleaned["$schema"]; present {
		t.Fatal("$schema must be dropped")
	}
	properties, _ := cleaned["properties"].(map[string]any)
	path, _ := properties["path"].(map[string]any)
	if _, present := path["$comment"]; present {
		t.Fatal("nested $comment must be dropped")
	}
	if path["pattern"] != "^/" {
		t.Fatal("a supported constraint must be preserved")
	}
	paths, _ := properties["paths"].(map[string]any)
	items, _ := paths["items"].(map[string]any)
	if _, present := items["uniqueItems"]; present {
		t.Fatal("nested uniqueItems must be dropped")
	}
}

// TestBuildGeminiRequestMapsGenerationConfig pins the output token limit and
// that thinking is not configured into a request that expressed no preference.
func TestBuildGeminiRequestMapsGenerationConfig(t *testing.T) {
	body := buildGeminiRequest(StreamRequest{
		Messages:  []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
		MaxTokens: 4096,
	}, "gemini-3.5-flash-lite")
	config := body.GenerationConfig
	if config == nil || config.MaxOutputTokens != 4096 {
		t.Fatalf("generationConfig = %#v", config)
	}
	// No level chosen means the model's own documented default applies, but
	// thought summaries are still requested so reasoning can be shown.
	if config.ThinkingConfig == nil || !config.ThinkingConfig.IncludeThoughts {
		t.Fatalf("thinkingConfig = %#v", config.ThinkingConfig)
	}
	if config.ThinkingConfig.ThinkingLevel != "" {
		t.Fatalf("no level was chosen, so none may be sent: %q", config.ThinkingConfig.ThinkingLevel)
	}
}

// TestGeminiThinkingLevelPerModel pins Google's documented support table. Sending
// an unsupported level is a hard 400, so an unlisted level must be clamped to
// something the model accepts rather than forwarded.
func TestGeminiThinkingLevelPerModel(t *testing.T) {
	cases := []struct {
		model     string
		requested ReasonLevel
		want      string
	}{
		// Flash-Lite accepts all four levels.
		{"gemini-3.5-flash-lite", ReasonMinimal, "minimal"},
		{"gemini-3.5-flash-lite", ReasonLow, "low"},
		{"gemini-3.5-flash-lite", ReasonMedium, "medium"},
		{"gemini-3.5-flash-lite", ReasonHigh, "high"},
		// Ceraa's finer levels collapse onto Gemini's highest.
		{"gemini-3.5-flash-lite", ReasonXHigh, "high"},
		{"gemini-3.5-flash-lite", ReasonMax, "high"},
		// 3.7 and 3.8 Flash reject minimal outright.
		{"gemini-3.7-flash", ReasonMinimal, "low"},
		{"gemini-3.8-flash", ReasonMinimal, "low"},
		// Pro models never accept minimal, and thinking cannot be disabled.
		{"gemini-3.1-pro-preview", ReasonMinimal, "low"},
		{"gemini-3.1-pro-preview", ReasonMedium, "medium"},
		{"gemini-3-pro-preview", ReasonMedium, "high"},
		{"gemini-3-pro-preview", ReasonLow, "low"},
		// Unknown Gemini 3 model gets the full set.
		{"gemini-3.9-flash", ReasonMedium, "medium"},
	}
	for _, tc := range cases {
		got := buildGeminiThinkingConfig(tc.model, tc.requested)
		if got == nil || got.ThinkingLevel != tc.want {
			t.Errorf("buildGeminiThinkingConfig(%q, %q).ThinkingLevel = %v, want %q", tc.model, tc.requested, got, tc.want)
		}
	}
}

// TestGeminiThinkingLevelOffStillRequestsThoughts pins that turning thinking off
// is the only thing that suppresses thought summaries.
func TestGeminiThinkingLevelOffStillRequestsThoughts(t *testing.T) {
	off := buildGeminiThinkingConfig("gemini-3.5-flash-lite", ReasonOff)
	if off == nil {
		t.Fatal("a config is always sent so the choice is explicit")
	}
	if off.IncludeThoughts {
		t.Fatal("thinking off must not request thought summaries")
	}
	if off.ThinkingLevel != "" || off.ThinkingBudget != nil {
		t.Fatalf("thinking off must not pin a level: %#v", off)
	}
}

// TestGemini25UsesThinkingBudget pins that the 2.5 series is sent a token budget
// rather than a level, since it does not support levels at all, and that the two
// are never sent together.
func TestGemini25UsesThinkingBudget(t *testing.T) {
	config := buildGeminiThinkingConfig("gemini-2.5-pro", ReasonHigh)
	if config.ThinkingLevel != "" {
		t.Fatalf("2.5 rejects thinkingLevel: %q", config.ThinkingLevel)
	}
	if config.ThinkingBudget == nil || *config.ThinkingBudget != -1 {
		t.Fatalf("budget = %#v, want dynamic thinking", config.ThinkingBudget)
	}
	minimal := buildGeminiThinkingConfig("gemini-2.5-flash", ReasonMinimal)
	if minimal.ThinkingBudget == nil || *minimal.ThinkingBudget != 0 {
		t.Fatalf("minimal budget = %#v", minimal.ThinkingBudget)
	}
}

// TestBuildGeminiRequestMapsToolChoice pins that only choices Gemini names are
// translated; an automatic choice needs no toolConfig at all.
func TestBuildGeminiRequestMapsToolChoice(t *testing.T) {
	tools := []ToolDef{{Name: "bash", Description: "run", Parameters: map[string]any{"type": "object"}}}
	if body := buildGeminiRequest(StreamRequest{Tools: tools, ToolChoice: "auto"}, "gemini-3.5-flash-lite"); body.ToolConfig != nil {
		t.Fatalf("auto must send no toolConfig, got %#v", body.ToolConfig)
	}
	if body := buildGeminiRequest(StreamRequest{Tools: tools, ToolChoice: "required"}, "gemini-3.5-flash-lite"); body.ToolConfig == nil || body.ToolConfig.FunctionCallingConfig["mode"] != "ANY" {
		t.Fatalf("required = %#v", body.ToolConfig)
	}
	if body := buildGeminiRequest(StreamRequest{Tools: tools, ToolChoice: "none"}, "gemini-3.5-flash-lite"); body.ToolConfig == nil || body.ToolConfig.FunctionCallingConfig["mode"] != "NONE" {
		t.Fatalf("none = %#v", body.ToolConfig)
	}
}

// TestReadGeminiStreamPreservesSignatureAndUsage pins the wire format coming
// back: a signature attached to a functionCall part must survive, and
// usageMetadata must map onto Ceraa's usage fields.
func TestReadGeminiStreamPreservesSignatureAndUsage(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"responseId":"resp-1","modelVersion":"gemini-3-pro-preview","candidates":[{"content":{"parts":[{"text":"thinking about it","thought":true}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"text":"I will check."}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"bash","args":{"command":"ls"},"id":"call-1"},"thoughtSignature":"sig-real"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"thoughtsTokenCount":8,"cachedContentTokenCount":5,"totalTokenCount":128}}`,
		"",
	}, "\n")

	var reasoning, content, finish string
	var calls []StreamedToolCall
	var usage *UsageInfo
	client := &StreamingClient{}
	err := client.readGeminiStream(strings.NewReader(stream), func(delta ProviderResponse) {
		reasoning += delta.Reasoning
		content += delta.Content
		if delta.FinishReason != "" {
			finish = delta.FinishReason
		}
		calls = append(calls, delta.ToolCalls...)
		if delta.Usage != nil {
			usage = delta.Usage
		}
	})
	if err != nil {
		t.Fatalf("readGeminiStream: %v", err)
	}
	if reasoning != "thinking about it" {
		t.Fatalf("reasoning = %q", reasoning)
	}
	if content != "I will check." {
		t.Fatalf("content = %q", content)
	}
	if finish != "stop" {
		t.Fatalf("finish = %q, want stop", finish)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %#v", calls)
	}
	if calls[0].ThoughtSignature != "sig-real" {
		t.Fatalf("signature = %q, want sig-real", calls[0].ThoughtSignature)
	}
	if calls[0].ID != "call-1" || calls[0].Name != "bash" || !strings.Contains(calls[0].Arguments, `"command":"ls"`) {
		t.Fatalf("call = %#v", calls[0])
	}
	if usage == nil || usage.InputTokens != 100 || usage.OutputTokens != 20 || usage.ReasoningTokens != 8 || usage.CachedInputTokens != 5 || usage.TotalTokens != 128 {
		t.Fatalf("usage = %#v", usage)
	}
}

// TestReadGeminiStreamRequiresFinishReason pins that a stream which ends without
// telling us why is a retryable error rather than a silently short answer.
func TestReadGeminiStreamRequiresFinishReason(t *testing.T) {
	client := &StreamingClient{}
	err := client.readGeminiStream(strings.NewReader("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n"), func(ProviderResponse) {})
	if err == nil {
		t.Fatal("expected an error when the stream ends without a finish reason")
	}
}

// TestDescribeGeminiTurnNeverLeaksContent pins the diagnostic contract: it names
// every function call and its signature state without ever writing content.
func TestDescribeGeminiTurnNeverLeaksContent(t *testing.T) {
	contents, _ := encodeGeminiContents([]Item{
		{Kind: "text", Role: RoleUser, Text: "my secret question"},
		{Kind: "tool_calls", ToolCalls: []ToolCall{
			{ID: "c1", Name: "bash", Args: `{"command":"cat secret.txt"}`, ThoughtSignature: "sig-real-value"},
			{ID: "c2", Name: "read"},
		}},
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "bash", Output: "secret file contents"},
	})
	got := describeGeminiTurn(contents)
	for _, want := range []string{"user", "model/functionCall(bash:sig14,read:none)", "user/functionResponse*2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("turn %q missing %q", got, want)
		}
	}
	for _, leak := range []string{"my secret question", "cat secret.txt", "secret file contents"} {
		if strings.Contains(got, leak) {
			t.Fatalf("turn leaked content %q: %s", leak, got)
		}
	}
}

// TestStreamRoutesToNativeTransport pins the seam that decides which wire format
// is used, by pointing both transports at a local server and observing the path
// and payload each one produces.
func TestStreamRoutesToNativeTransport(t *testing.T) {
	items := []Item{
		{Kind: "text", Role: RoleSystem, Text: "You are Ceraa."},
		{Kind: "text", Role: RoleUser, Text: "list files"},
	}

	t.Run("native", func(t *testing.T) {
		var gotPath string
		var gotBody map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path + "?" + r.URL.RawQuery
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			if r.Header.Get("x-goog-api-key") != "test-key" {
				t.Errorf("x-goog-api-key = %q", r.Header.Get("x-goog-api-key"))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n")
		}))
		defer server.Close()

		client := New(ProviderConfig{
			BaseURL:      server.URL,
			APIKey:       "test-key",
			Model:        "gemini-3-pro-preview",
			GeminiNative: true,
			HTTPClient:   server.Client(),
		})
		var content string
		if err := client.Stream(context.Background(), StreamRequest{Messages: items}, func(delta ProviderResponse) {
			content += delta.Content
		}); err != nil {
			t.Fatalf("native stream: %v", err)
		}
		if content != "ok" {
			t.Fatalf("content = %q", content)
		}
		if !strings.Contains(gotPath, ":streamGenerateContent") || !strings.Contains(gotPath, "alt=sse") {
			t.Fatalf("native path = %q", gotPath)
		}
		if _, present := gotBody["contents"]; !present {
			t.Fatalf("native body = %#v", gotBody)
		}
		if _, present := gotBody["systemInstruction"]; !present {
			t.Fatalf("system prompt must use systemInstruction: %#v", gotBody)
		}
	})

	t.Run("compatibility", func(t *testing.T) {
		var gotPath string
		var gotBody map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		defer server.Close()

		client := New(ProviderConfig{
			BaseURL:    server.URL,
			APIKey:     "test-key",
			Model:      "gemini-3-pro-preview",
			HTTPClient: server.Client(),
		})
		var content string
		if err := client.Stream(context.Background(), StreamRequest{Messages: items}, func(delta ProviderResponse) {
			content += delta.Content
		}); err != nil {
			t.Fatalf("compatibility stream: %v", err)
		}
		if content != "ok" {
			t.Fatalf("content = %q", content)
		}
		if !strings.HasSuffix(gotPath, "/chat/completions") {
			t.Fatalf("compatibility path = %q", gotPath)
		}
		if _, present := gotBody["messages"]; !present {
			t.Fatalf("compatibility body = %#v", gotBody)
		}
	})
}
