package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeCodexTokens struct {
	mu        sync.Mutex
	token     string
	refreshed int
	failNext  bool
}

func (f *fakeCodexTokens) AccessToken(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.token, nil
}

func (f *fakeCodexTokens) RefreshToken(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed++
	f.token = "refreshed-token"
	return f.token, nil
}

func (f *fakeCodexTokens) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshed
}

// codexServer is a stand-in for the Codex endpoint that records the request it
// received, so a test can assert on the wire shape rather than on internals.
type codexServer struct {
	mu       sync.Mutex
	bodies   []map[string]any
	headers  []http.Header
	paths    []string
	statuses []int
	replies  []string
}

func (c *codexServer) record(r *http.Request, body map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, body)
	c.headers = append(c.headers, r.Header.Clone())
	c.paths = append(c.paths, r.URL.Path)
}

func (c *codexServer) body(i int) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodies[i]
}

func (c *codexServer) reply(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i < len(c.replies) {
		return c.replies[i]
	}
	return ""
}

func (c *codexServer) lastBody(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("no request reached the fake endpoint")
	}
	return c.bodies[len(c.bodies)-1]
}

func newCodexClient(t *testing.T, server *codexServer, tokens CodexTokenSource) *StreamingClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		server.record(r, body)
		c := &codexServer{}
		_ = c
		server.mu.Lock()
		idx := len(server.bodies) - 1
		status := http.StatusOK
		if idx < len(server.statuses) {
			status = server.statuses[idx]
		}
		reply := ""
		if idx < len(server.replies) {
			reply = server.replies[idx]
		}
		server.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprint(w, reply)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, reply)
	}))
	t.Cleanup(srv.Close)
	return New(ProviderConfig{
		BaseURL:     srv.URL,
		Model:       "gpt-5.6-luna",
		CodexNative: true,
		CodexTokens: tokens,
		MaxRetries:  1,
		Timeout:     10 * time.Second,
	})
}

const textStream = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.6-luna\"}}\n\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hel\"}\n\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n" +
	"event: response.completed\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.6-luna\",\"status\":\"completed\",\"usage\":{\"input_tokens\":11,\"output_tokens\":3,\"total_tokens\":14}}}\n\n"

func collect(t *testing.T, client *StreamingClient, req StreamRequest) (string, ProviderResponse, error) {
	t.Helper()
	var content string
	var final ProviderResponse
	err := client.Stream(context.Background(), req, func(r ProviderResponse) {
		content += r.Content
		if r.Done {
			final = r
		}
	})
	return content, final, err
}

func TestCodexRequestShapeMatchesEndpointConstraints(t *testing.T) {
	server := &codexServer{replies: []string{textStream}}
	client := newCodexClient(t, server, &fakeCodexTokens{token: "tok"})

	_, _, err := collect(t, client, StreamRequest{
		Messages: []Item{
			{Kind: "text", Role: RoleSystem, Text: "You are terse."},
			{Kind: "text", Role: RoleUser, Text: "hi"},
		},
		Reason:         ReasonMedium,
		MaxTokens:      4096,
		PromptCacheKey: "thread-1",
		SessionID:      "sess-1",
		Tools:          []ToolDef{{Name: "get_weather", Description: "d", Parameters: map[string]any{"type": "object"}}},
		ToolChoice:     "",
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	body := server.lastBody(t)

	// The endpoint rejects these, so asserting their absence is the point.
	for _, forbidden := range []string{"max_output_tokens", "temperature", "top_p", "prompt_cache_options"} {
		if _, present := body[forbidden]; present {
			t.Errorf("body carries %q, which the endpoint rejects with 400", forbidden)
		}
	}
	if body["store"] != false {
		t.Errorf("store = %v, want false (endpoint requires it)", body["store"])
	}
	if body["stream"] != true {
		t.Errorf("stream = %v, want true (endpoint requires it)", body["stream"])
	}
	if body["model"] != "gpt-5.6-luna" {
		t.Errorf("model = %v", body["model"])
	}
	if body["instructions"] != "You are terse." {
		t.Errorf("instructions = %v, want the system prompt lifted out of input", body["instructions"])
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != string(ReasonMedium) {
		t.Errorf("reasoning = %v, want effort %q", body["reasoning"], ReasonMedium)
	}
	if body["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want auto", body["tool_choice"])
	}
	if body["prompt_cache_key"] != "thread-1" {
		t.Errorf("prompt_cache_key = %v", body["prompt_cache_key"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want one", body["tools"])
	}
	first, _ := tools[0].(map[string]any)
	if first["type"] != "function" || first["name"] != "get_weather" {
		t.Errorf("tool shape = %v, want a flat function tool", first)
	}

	// The system prompt must not also appear as an input message.
	input, _ := body["input"].([]any)
	for _, entry := range input {
		m, _ := entry.(map[string]any)
		if m["role"] == "system" || m["role"] == "developer" {
			t.Errorf("system prompt leaked into input: %v", m)
		}
	}
	if len(input) != 1 {
		t.Errorf("input = %v, want only the user turn", input)
	}
}

func TestCodexSendsBearerTokenAndAcceptHeader(t *testing.T) {
	server := &codexServer{replies: []string{textStream}}
	client := newCodexClient(t, server, &fakeCodexTokens{token: "secret-token"})

	if _, _, err := collect(t, client, StreamRequest{Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}}}); err != nil {
		t.Fatalf("stream: %v", err)
	}
	server.mu.Lock()
	header := server.headers[0]
	path := server.paths[0]
	server.mu.Unlock()

	if got := header.Get("Authorization"); got != "Bearer secret-token" {
		t.Errorf("Authorization = %q", got)
	}
	if got := header.Get("Accept"); !strings.Contains(got, "text/event-stream") {
		t.Errorf("Accept = %q, want an event stream", got)
	}
	if path != "/responses" {
		t.Errorf("path = %q, want /responses", path)
	}
}

func TestCodexStreamsTextAndUsage(t *testing.T) {
	server := &codexServer{replies: []string{textStream}}
	client := newCodexClient(t, server, &fakeCodexTokens{token: "tok"})

	content, final, err := collect(t, client, StreamRequest{Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if content != "Hello" {
		t.Errorf("content = %q, want Hello", content)
	}
	if !final.Done {
		t.Error("final response not marked done")
	}
	if final.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want stop", final.FinishReason)
	}
	if final.Usage == nil || final.Usage.InputTokens != 11 || final.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", final.Usage)
	}
	if final.ResponseID != "resp_1" {
		t.Errorf("response id = %q", final.ResponseID)
	}
}

func TestCodexEmitsFunctionCall(t *testing.T) {
	stream := "event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_7\",\"name\":\"get_weather\"}}\n\n" +
		"event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_7\",\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"Lagos\\\"}\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"status\":\"completed\"}}\n\n"
	server := &codexServer{replies: []string{stream}}
	client := newCodexClient(t, server, &fakeCodexTokens{token: "tok"})

	_, final, err := collect(t, client, StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "weather?"}},
		Tools:    []ToolDef{{Name: "get_weather", Description: "d", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if final.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q, want tool_calls", final.FinishReason)
	}
	if len(final.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want one", final.ToolCalls)
	}
	call := final.ToolCalls[0]
	if call.Name != "get_weather" || call.ID != "call_7" {
		t.Errorf("call = %+v", call)
	}
	if call.Arguments != `{"city":"Lagos"}` {
		t.Errorf("arguments = %q", call.Arguments)
	}
}

func TestCodexEncodesToolHistoryForResponsesAPI(t *testing.T) {
	input, instructions := codexInput([]Item{
		{Kind: "text", Role: RoleSystem, Text: "sys"},
		{Kind: "text", Role: RoleUser, Text: "weather?"},
		{Kind: "tool_call", CallID: "call_1", Name: "get_weather", Args: `{"city":"Lagos"}`},
		{Kind: "text", Role: RoleTool, CallID: "call_1", Name: "get_weather", Output: `{"temp_c":29}`},
		{Kind: "text", Role: RoleAssistant, Text: "It is 29C."},
	})
	if instructions != "sys" {
		t.Errorf("instructions = %q", instructions)
	}
	kinds := make([]string, 0, len(input))
	for _, m := range input {
		if m["type"] == "function_call" {
			kinds = append(kinds, "function_call:"+m["name"].(string)+":"+m["call_id"].(string))
			continue
		}
		if m["type"] == "function_call_output" {
			kinds = append(kinds, "function_call_output:"+m["call_id"].(string))
			continue
		}
		kinds = append(kinds, m["role"].(string))
	}
	want := []string{"user", "function_call:get_weather:call_1", "function_call_output:call_1", "assistant"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Errorf("input order = %v, want %v", kinds, want)
	}
}

// An interrupted run leaves a tool call with no recorded result. The Responses
// API rejects that turn, so the normaliser has to supply a result.
func TestCodexSuppliesResultForUnansweredToolCall(t *testing.T) {
	input, _ := codexInput([]Item{
		{Kind: "text", Role: RoleUser, Text: "weather?"},
		{Kind: "tool_call", CallID: "call_9", Name: "get_weather", Args: `{"city":"Lagos"}`},
	})
	var hasOutput bool
	for _, m := range input {
		if m["type"] == "function_call_output" && m["call_id"] == "call_9" {
			hasOutput = true
		}
	}
	if !hasOutput {
		t.Errorf("no synthetic result for the unanswered call; input = %v", input)
	}
}

func TestCodexDuplicateToolResultsCollapse(t *testing.T) {
	input, _ := codexInput([]Item{
		{Kind: "text", Role: RoleUser, Text: "weather?"},
		{Kind: "tool_call", CallID: "call_1", Name: "get_weather", Args: `{}`},
		{Kind: "text", Role: RoleTool, CallID: "call_1", Output: "first"},
		{Kind: "text", Role: RoleTool, CallID: "call_1", Output: "second"},
	})
	count := 0
	for _, m := range input {
		if m["type"] == "function_call_output" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("function_call_output count = %d, want 1", count)
	}
}

func TestCodexFlatErrorEnvelope(t *testing.T) {
	server := &codexServer{
		statuses: []int{http.StatusBadRequest},
		replies:  []string{`{"detail":"Unsupported parameter: temperature"}`},
	}
	client := newCodexClient(t, server, &fakeCodexTokens{token: "tok"})

	_, _, err := collect(t, client, StreamRequest{Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}}})
	if err == nil {
		t.Fatal("expected an error")
	}
	perr, ok := err.(*ProviderError)
	if !ok {
		t.Fatalf("error type = %T, want *ProviderError", err)
	}
	if perr.Status != http.StatusBadRequest {
		t.Errorf("status = %d", perr.Status)
	}
	if !strings.Contains(perr.Body, "Unsupported parameter") {
		t.Errorf("body = %q, want the detail message surfaced", perr.Body)
	}
}

func TestCodexNestedErrorEnvelope(t *testing.T) {
	server := &codexServer{
		statuses: []int{http.StatusNotFound},
		replies:  []string{`{"error":{"message":"The model ` + "`x`" + ` does not exist","code":"model_not_found"}}`},
	}
	client := newCodexClient(t, server, &fakeCodexTokens{token: "tok"})

	_, _, err := collect(t, client, StreamRequest{Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}}})
	perr, ok := err.(*ProviderError)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if !strings.Contains(perr.Body, "does not exist") || !strings.Contains(perr.Body, "model_not_found") {
		t.Errorf("body = %q, want message and code", perr.Body)
	}
}

// A token revoked before its recorded expiry is only recoverable by forcing a
// fresh exchange and replaying the request once.
func TestCodexUnauthorizedForcesRefreshAndRetriesOnce(t *testing.T) {
	server := &codexServer{
		statuses: []int{http.StatusUnauthorized, http.StatusOK},
		replies:  []string{`{"detail":"invalid token"}`, textStream},
	}
	tokens := &fakeCodexTokens{token: "stale"}
	client := newCodexClient(t, server, tokens)

	content, _, err := collect(t, client, StreamRequest{Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if content != "Hello" {
		t.Errorf("content = %q, want the retried response", content)
	}
	if tokens.count() != 1 {
		t.Errorf("refresh count = %d, want exactly 1", tokens.count())
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if got := server.headers[1].Get("Authorization"); got != "Bearer refreshed-token" {
		t.Errorf("retry Authorization = %q, want the refreshed token", got)
	}
}

func TestCodexRefusesWithoutCredential(t *testing.T) {
	server := &codexServer{replies: []string{textStream}}
	client := newCodexClient(t, server, &fakeCodexTokens{token: ""})

	_, _, err := collect(t, client, StreamRequest{Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}}})
	if err == nil {
		t.Fatal("expected an error with no credential stored")
	}
	if !strings.Contains(err.Error(), "/codex-login") {
		t.Errorf("error = %v, want it to name the login command", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.bodies) != 0 {
		t.Error("an unauthenticated request was sent anyway")
	}
}

func TestCodexStreamErrorSurfaces(t *testing.T) {
	stream := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_3\"}}\n\n" +
		"event: error\n" +
		"data: {\"type\":\"error\",\"error\":{\"message\":\"upstream exploded\"}}\n\n"
	server := &codexServer{replies: []string{stream}}
	client := newCodexClient(t, server, &fakeCodexTokens{token: "tok"})

	_, _, err := collect(t, client, StreamRequest{Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("error = %v, want the stream error message", err)
	}
}

func TestCodexReasoningEventsAreSurfaced(t *testing.T) {
	stream := "event: response.reasoning_summary_text.delta\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thinking...\"}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_4\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"output_tokens_details\":{\"reasoning_tokens\":7}}}}\n\n"
	server := &codexServer{replies: []string{stream}}
	client := newCodexClient(t, server, &fakeCodexTokens{token: "tok"})

	var reasoning string
	var final ProviderResponse
	err := client.Stream(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(r ProviderResponse) {
		reasoning += r.Reasoning
		if r.Done {
			final = r
		}
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if reasoning != "thinking..." {
		t.Errorf("reasoning = %q", reasoning)
	}
	if final.Usage == nil || final.Usage.ReasoningTokens != 7 {
		t.Errorf("reasoning tokens = %+v", final.Usage)
	}
}

func TestCodexURLResolution(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"https://chatgpt.com/backend-api/codex", "https://chatgpt.com/backend-api/codex/responses"},
		{"https://chatgpt.com/backend-api/codex/", "https://chatgpt.com/backend-api/codex/responses"},
		{"https://chatgpt.com/backend-api/codex/responses", "https://chatgpt.com/backend-api/codex/responses"},
		{"http://127.0.0.1:8080/codex", "http://127.0.0.1:8080/codex/responses"},
	} {
		if got := codexResponsesURL(tc.base); got != tc.want {
			t.Errorf("codexResponsesURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

func TestUsesCodexNative(t *testing.T) {
	if !UsesCodexNative("https://chatgpt.com/backend-api/codex") {
		t.Error("codex endpoint not detected")
	}
	if !UsesCodexNative("https://chatgpt.com/backend-api/codex/responses") {
		t.Error("codex responses URL not detected")
	}
	if UsesCodexNative("https://api.openai.com/v1") {
		t.Error("plain OpenAI API must not be treated as Codex")
	}
	if UsesCodexNative("https://generativelanguage.googleapis.com/v1beta") {
		t.Error("Gemini must not be treated as Codex")
	}
	if UsesCodexNative("") {
		t.Error("empty URL must not be treated as Codex")
	}
}

func TestIsCodexModel(t *testing.T) {
	if !IsCodexModel("gpt-5.6-luna") || !IsCodexModel("gpt-5.6-terra") {
		t.Error("known codex models rejected")
	}
	// These are the IDs another client still ships; the live endpoint refuses
	// them, so accepting them here would produce a guaranteed 400.
	for _, retired := range []string{"gpt-5.5", "gpt-5.6-sol", "gpt-6-sol", "gpt-5-codex", "codex-mini-latest"} {
		if IsCodexModel(retired) {
			t.Errorf("IsCodexModel(%q) = true, but the endpoint rejects it", retired)
		}
	}
}
