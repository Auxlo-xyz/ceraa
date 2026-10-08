package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Messages API is unforgiving about shape, and every one of these is a way it
// answers 400. The request builder is the part that has to be right, so these
// assert the body rather than the streaming behaviour.
func TestMessagesRequestShape(t *testing.T) {
	cfg := ProviderConfig{Model: "MiniMax-M3", BaseURL: "https://api.minimax.io/anthropic"}
	items := []Item{
		{Kind: "text", Role: RoleSystem, Text: "you are a careful agent"},
		{Kind: "text", Role: RoleUser, Text: "read the file"},
		{Kind: "tool_call", CallID: "c1", Name: "read", Args: `{"path":"/tmp/x"}`},
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "read", Output: "contents"},
	}
	tools := []ToolDef{{Name: "read", Description: "read a file", Parameters: map[string]any{"type": "object"}}}

	messages, err := encodeAnthropicMessages(items)
	if err != nil {
		t.Fatal(err)
	}
	system := anthropicSystemBlocks(joinSystemText(items), 0, cfg)
	convertedTools := anthropicTools(tools, cfg)
	body := map[string]any{
		"model": cfg.Model, "messages": messages, "max_tokens": 1024, "stream": true,
		"system": system, "tools": convertedTools,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(encoded)

	// The system prompt is a top-level argument, never a turn. Checking for a
	// "system" role is not enough: a leaked item arrives as a user turn, so the
	// count of the text is what actually detects it.
	if strings.Count(raw, "you are a careful agent") != 1 {
		t.Errorf("the system prompt appears %d times, want 1; it is being sent as a turn as well:\n%s",
			strings.Count(raw, "you are a careful agent"), raw)
	}
	if !strings.Contains(raw, `"system":`) {
		t.Errorf("no top-level system argument: %s", raw)
	}
	// A tool call is an assistant turn with a tool_use block.
	if !strings.Contains(raw, `"tool_use"`) || !strings.Contains(raw, `"role":"assistant"`) {
		t.Errorf("the tool call is not an assistant tool_use turn: %s", raw)
	}
	// A result is a user turn with a tool_result block, and it must follow its call.
	if !strings.Contains(raw, `"tool_result"`) {
		t.Errorf("the tool result is not a tool_result block: %s", raw)
	}
	if strings.Index(raw, `"tool_use"`) > strings.Index(raw, `"tool_result"`) {
		t.Errorf("the result precedes its call: %s", raw)
	}
	// The Messages API authenticates with x-api-key, not a bearer token, so the
	// request must not carry an Authorization header.
	if strings.Contains(raw, "Authorization") {
		t.Errorf("the body carries an Authorization field: %s", raw)
	}
	// Tool schemas use input_schema, not parameters.
	if !strings.Contains(raw, `"input_schema"`) {
		t.Errorf("tool schema is not input_schema: %s", raw)
	}
}

// Explicit mode is the reason this transport exists: the breakpoints written
// earlier had no transport able to carry them.
func TestExplicitCacheBreakpointsReachTheWire(t *testing.T) {
	items := []Item{{Kind: "text", Role: RoleSystem, Text: "stable text\n\nvolatile tail"}}
	seam := len("stable text\n\n")
	cfg := ProviderConfig{PromptCacheMode: "explicit", PromptCacheTTL: "1h"}

	blocks := anthropicSystemBlocks(joinSystemText(items), seam, cfg)
	if len(blocks) == 0 {
		t.Fatal("no system blocks")
	}
	last := blocks[len(blocks)-1]
	control, ok := last["cache_control"].(map[string]any)
	if !ok {
		t.Fatalf("the last system block carries no cache_control: %v", last)
	}
	if control["type"] != "ephemeral" {
		t.Errorf("cache_control type = %v, want ephemeral", control["type"])
	}
	if control["ttl"] != "1h" {
		t.Errorf("cache_control ttl = %v, want 1h", control["ttl"])
	}
}

// Auto mode must not add breakpoints. Turning a content string into a block array
// changes the request shape for endpoints that may not accept it.
func TestAutoModeAddsNoCacheControl(t *testing.T) {
	items := []Item{{Kind: "text", Role: RoleSystem, Text: "plain"}}
	blocks := anthropicSystemBlocks(joinSystemText(items), 0, ProviderConfig{PromptCacheMode: "auto"})
	for _, block := range blocks {
		if _, ok := block["cache_control"]; ok {
			t.Errorf("auto mode added a cache_control: %v", block)
		}
	}
}

// The tool schemas are the largest stable block, so the breakpoint goes on the
// last tool and not the first.
func TestToolSchemasCarryTheBreakpoint(t *testing.T) {
	tools := []ToolDef{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	converted := anthropicTools(tools, ProviderConfig{PromptCacheMode: "explicit", PromptCacheTTL: "5m"})
	if len(converted) != 3 {
		t.Fatalf("got %d tools", len(converted))
	}
	if _, ok := converted[0]["cache_control"]; ok {
		t.Error("the first tool carries the breakpoint, so the later schemas are not cached")
	}
	last := converted[len(converted)-1]
	control, ok := last["cache_control"].(map[string]any)
	if !ok {
		t.Fatalf("the last tool carries no cache_control: %v", last)
	}
	// 5m is the provider default, so it is omitted rather than restated; only 1h
	// is sent explicitly, because that is the one that is not the default.
	if _, present := control["ttl"]; present {
		t.Errorf("the default 5m ttl was sent explicitly: %v", control)
	}
}

// The API has no "none". Sending the literal string is a 400, so it is omitted.
func TestToolChoiceNoneIsOmitted(t *testing.T) {
	if choice := anthropicToolChoice("none"); choice != nil {
		t.Errorf("tool_choice none produced %v; it must be omitted", choice)
	}
	if choice := anthropicToolChoice("auto"); choice == nil {
		t.Error("auto was omitted")
	}
	if choice := anthropicToolChoice("required"); choice == nil {
		t.Error("required was omitted")
	}
}

func TestMessagesURLResolution(t *testing.T) {
	cases := map[string]string{
		"https://api.minimax.io/anthropic":         "https://api.minimax.io/anthropic/v1/messages",
		"https://api.minimax.io/anthropic/":        "https://api.minimax.io/anthropic/v1/messages",
		"https://api.anthropic.com":                "https://api.anthropic.com/v1/messages",
		"https://api.minimax.io/anthropic/v1/mess": "https://api.minimax.io/anthropic/v1/mess/v1/messages",
	}
	for base, want := range cases {
		if got := anthropicMessagesURL(base); got != want {
			t.Errorf("anthropicMessagesURL(%q) = %q, want %q", base, got, want)
		}
	}
}

// Streaming is the other half: text, reasoning and tool input all arrive as
// different delta types and have to end up in the right field.
func TestStreamEmitsTextReasoningAndToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"MiniMax-M3"},"usage":{"input_tokens":120,"cache_read_input_tokens":100,"output_tokens":0}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello "}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"/tmp/x\"}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","usage":{"output_tokens":42}}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n"

	var text, reasoning strings.Builder
	var calls []StreamedToolCall
	var usage *UsageInfo
	client := &StreamingClient{}
	err := client.readAnthropicStream(strings.NewReader(stream), func(r ProviderResponse) {
		text.WriteString(r.Content)
		reasoning.WriteString(r.Reasoning)
		calls = append(calls, r.ToolCalls...)
		if r.Usage != nil {
			usage = r.Usage
		}
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if text.String() != "hello world" {
		t.Errorf("text = %q, want %q", text.String(), "hello world")
	}
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if calls[0].Name != "read" {
		t.Errorf("tool name = %q", calls[0].Name)
	}
	// Partial JSON arrives in fragments and must be rejoined into exactly one
	// value. This was checked with Contains, which passed for {}{"path":"/tmp/x"}:
	// content_block_start carries input:{} and the fragments were appended to it,
	// so the arguments were two top-level values and the follow-up request died
	// in encoding/json with "invalid character '{' after top-level value".
	if calls[0].Arguments != `{"path":"/tmp/x"}` {
		t.Errorf("tool arguments = %q, want %q", calls[0].Arguments, `{"path":"/tmp/x"}`)
	}
	if !json.Valid([]byte(calls[0].Arguments)) {
		t.Errorf("tool arguments %q are not one JSON value", calls[0].Arguments)
	}
	if usage == nil {
		t.Fatal("no usage reported")
	}
	if usage.InputTokens != 120 {
		t.Errorf("input tokens = %d, want 120", usage.InputTokens)
	}
	// Cached tokens are what the whole accounting path depends on.
	if usage.CachedInputTokens != 100 {
		t.Errorf("cached input tokens = %d, want 100", usage.CachedInputTokens)
	}
	if usage.OutputTokens != 42 {
		t.Errorf("output tokens = %d, want 42", usage.OutputTokens)
	}
}

// MiniMax sends usage: null on message_start, which the Hermes agent had to work
// around. A null must not clobber counts that already arrived, and a second real
// payload must not be double counted.
func TestNullUsageDoesNotClobberCounts(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m"},"usage":{"input_tokens":50,"output_tokens":0}}`,
		`data: {"type":"message_delta","usage":null}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n"

	var usage *UsageInfo
	client := &StreamingClient{}
	if err := client.readAnthropicStream(strings.NewReader(stream), func(r ProviderResponse) {
		if r.Usage != nil {
			usage = r.Usage
		}
	}); err != nil {
		t.Fatal(err)
	}
	if usage == nil || usage.InputTokens != 50 {
		t.Errorf("usage = %+v, want input tokens 50 preserved through a null event", usage)
	}
}

// Error bodies differ between Anthropic and a compatibility host, so both are read.
func TestAnthropicErrorMessage(t *testing.T) {
	cases := map[string]string{
		`{"error":{"type":"overloaded_error","message":"Overloaded"}}`: "Overloaded",
		`{"error":{"message":"bad model"}}`:                            "bad model",
		`{"message":"flat shape"}`:                                     "flat shape",
	}
	for body, want := range cases {
		if got := anthropicErrorMessage([]byte(body)); got != want {
			t.Errorf("anthropicErrorMessage(%s) = %q, want %q", body, got, want)
		}
	}
	if got := anthropicErrorMessage(nil); got == "" {
		t.Error("an empty error body produced an empty message")
	}
}

// A duplicate result for one call id, which an interrupted session produces, must
// be collapsed rather than sent twice.
func TestDuplicateToolResultsAreCollapsed(t *testing.T) {
	items := []Item{
		{Kind: "text", Role: RoleUser, Text: "go"},
		{Kind: "tool_call", CallID: "c1", Name: "read", Args: `{}`},
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "read", Output: "first"},
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "read", Output: "second"},
	}
	messages, err := encodeAnthropicMessages(items)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(messages)
	raw := string(encoded)
	if strings.Count(raw, `"tool_result"`) != 1 {
		t.Errorf("expected one tool_result, got %d: %s", strings.Count(raw, `"tool_result"`), raw)
	}
}

// A live request against a stub, so the headers are actually observed. The body
// assertions cannot see them at all, which is why sending a bearer token instead
// of x-api-key passed every test.
func TestMessagesRequestHeaders(t *testing.T) {
	var got http.Header
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	client := &StreamingClient{
		cfg: ProviderConfig{
			Model:           "MiniMax-M3",
			BaseURL:         srv.URL,
			AnthropicNative: true,
			AnthropicTokens: stubTokens{token: "sk-test"},
			MaxRetries:      0,
		},
		http: srv.Client(),
	}
	err := client.streamAnthropic(context.Background(), StreamRequest{
		Messages:  []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
		MaxTokens: 16,
	}, func(ProviderResponse) {})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	// The Messages API authenticates with x-api-key plus an explicit API version.
	// A bearer token is a 401 on Anthropic and a differently shaped error elsewhere.
	if got.Get("x-api-key") != "sk-test" {
		t.Errorf("x-api-key = %q, want the credential; headers were %v", got.Get("x-api-key"), got)
	}
	if got.Get("Authorization") != "" {
		t.Errorf("an Authorization header was also sent: %q", got.Get("Authorization"))
	}
	if got.Get("Anthropic-Version") == "" {
		t.Error("anthropic-version was not sent; the API defaults to a version that may not accept this request shape")
	}
	if len(body) == 0 {
		t.Error("no request body was sent")
	}
}

type stubTokens struct{ token string }

func (s stubTokens) AccessToken(context.Context) (string, error) { return s.token, nil }

// A stream that reports no usage must produce nil, not a zero-valued struct. A
// zero struct reads as "the provider reported 0 tokens", which is exactly the
// condition the usage-trust guard rejects, so a provider that simply omits usage
// would look like a misreporting one.
func TestStreamWithNoUsageReportsNil(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m"},"usage":null}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`data: {"type":"message_delta","usage":null}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n"

	var usage *UsageInfo
	sawZero := false
	client := &StreamingClient{}
	if err := client.readAnthropicStream(strings.NewReader(stream), func(r ProviderResponse) {
		if r.Usage != nil {
			usage = r.Usage
			if r.Usage.InputTokens == 0 && r.Usage.OutputTokens == 0 {
				sawZero = true
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if usage != nil && sawZero {
		t.Errorf("a stream with no usage reported a zero struct: %+v", usage)
	}
}

// A host that reports input and cached tokens on both message_start and
// message_delta must not have them added together. This is the same double-count
// the usage trust guard exists to catch, arriving from the other direction.
func TestRepeatedUsageIsAssignedNotSummed(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m"},"usage":{"input_tokens":120,"cache_read_input_tokens":100,"output_tokens":1}}`,
		`data: {"type":"message_delta","usage":{"input_tokens":120,"cache_read_input_tokens":100,"output_tokens":42}}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n"

	var usage *UsageInfo
	client := &StreamingClient{}
	if err := client.readAnthropicStream(strings.NewReader(stream), func(r ProviderResponse) {
		if r.Usage != nil {
			usage = r.Usage
		}
	}); err != nil {
		t.Fatal(err)
	}
	if usage == nil {
		t.Fatal("no usage reported")
	}
	if usage.InputTokens != 120 {
		t.Errorf("input tokens = %d, want 120; the repeated counts were summed", usage.InputTokens)
	}
	if usage.CachedInputTokens != 100 {
		t.Errorf("cached input tokens = %d, want 100; the repeated counts were summed", usage.CachedInputTokens)
	}
	if usage.OutputTokens != 42 {
		t.Errorf("output tokens = %d, want 42", usage.OutputTokens)
	}
}

// When the two events disagree, the later one wins. Proves the merge replaces
// rather than keeping the first reading, which identical values cannot show.
func TestLaterUsageReplacesTheEarlier(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m"},"usage":{"input_tokens":10,"cache_read_input_tokens":0}}`,
		`data: {"type":"message_delta","usage":{"input_tokens":120,"cache_read_input_tokens":100,"cache_creation_input_tokens":40}}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n"

	var usage *UsageInfo
	client := &StreamingClient{}
	if err := client.readAnthropicStream(strings.NewReader(stream), func(r ProviderResponse) {
		if r.Usage != nil {
			usage = r.Usage
		}
	}); err != nil {
		t.Fatal(err)
	}
	if usage == nil {
		t.Fatal("no usage reported")
	}
	if usage.InputTokens != 120 {
		t.Errorf("input tokens = %d, want the later value 120", usage.InputTokens)
	}
	if usage.CachedInputTokens != 100 {
		t.Errorf("cached input tokens = %d, want the later value 100", usage.CachedInputTokens)
	}
	// Cache writes only reach their assignment on the second event, so a test that
	// only sends one cannot see that line fail.
	if usage.CacheWriteTokens != 40 {
		t.Errorf("cache write tokens = %d, want the later value 40", usage.CacheWriteTokens)
	}
}

// Cache writes are what explicit mode is trying to buy, so they have to survive
// the parse or a 1h TTL reports no cost at all.
func TestCacheWriteTokensAreReported(t *testing.T) {
	stream := `data: {"type":"message_start","message":{"id":"m"},"usage":{"input_tokens":120,"cache_read_input_tokens":100,"cache_creation_input_tokens":40}}` + "\n\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	var usage *UsageInfo
	client := &StreamingClient{}
	if err := client.readAnthropicStream(strings.NewReader(stream), func(r ProviderResponse) {
		if r.Usage != nil {
			usage = r.Usage
		}
	}); err != nil {
		t.Fatal(err)
	}
	if usage == nil || usage.CacheWriteTokens != 40 {
		t.Errorf("cache write tokens = %+v, want 40", usage)
	}
}

// Thinking arrives as its own delta type and lands in Reasoning, not Content.
// Putting it in Content would show the model its own scratchwork as prose.
func TestThinkingDeltaIsSeparatedFromText(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n"

	var text, reasoning strings.Builder
	client := &StreamingClient{}
	if err := client.readAnthropicStream(strings.NewReader(stream), func(r ProviderResponse) {
		text.WriteString(r.Content)
		reasoning.WriteString(r.Reasoning)
	}); err != nil {
		t.Fatal(err)
	}
	if reasoning.String() != "let me think" {
		t.Errorf("reasoning = %q", reasoning.String())
	}
	if text.String() != "answer" {
		t.Errorf("text = %q; reasoning must not leak into the visible answer", text.String())
	}
}

// A rate-limited or gateway-failed request must be retried, or a run dies on the
// first blip. The retry count and the wait are asserted through call count.
func TestRetryableStatusIsRetried(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable} {
		var calls int
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls++
			attempt := calls
			mu.Unlock()
			if attempt == 1 {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"try later"}}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
		}))
		client := &StreamingClient{
			cfg: ProviderConfig{
				Model: "MiniMax-M3", BaseURL: srv.URL,
				AnthropicNative: true, AnthropicTokens: stubTokens{token: "k"},
				MaxRetries: 2, RetryBackoff: time.Millisecond,
			},
			http: srv.Client(),
		}
		err := client.streamAnthropic(context.Background(), StreamRequest{
			Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}}, MaxTokens: 8,
		}, func(ProviderResponse) {})
		srv.Close()
		mu.Lock()
		got := calls
		mu.Unlock()
		if err != nil {
			t.Errorf("status %d: request failed instead of retrying: %v", status, err)
		}
		if got < 2 {
			t.Errorf("status %d: made %d request(s), want a retry", status, got)
		}
	}
}

// A permanent failure must not be retried, or a bad request is hammered.
func TestPermanentStatusIsNotRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad model"}}`))
	}))
	defer srv.Close()
	client := &StreamingClient{
		cfg: ProviderConfig{
			Model: "MiniMax-M3", BaseURL: srv.URL,
			AnthropicNative: true, AnthropicTokens: stubTokens{token: "k"},
			MaxRetries: 2, RetryBackoff: time.Millisecond,
		},
		http: srv.Client(),
	}
	err := client.streamAnthropic(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}}, MaxTokens: 8,
	}, func(ProviderResponse) {})
	if err == nil {
		t.Fatal("a 400 was reported as success")
	}
	if calls != 1 {
		t.Errorf("a 400 was retried %d times; it cannot succeed on a second attempt", calls-1)
	}
	if !strings.Contains(err.Error(), "bad model") {
		t.Errorf("the error lost the provider's detail: %v", err)
	}
}

// The effort is an output_config field. reasoning.effort is accepted and ignored,
// which is a silent way to send a setting that looks like it works.
func TestAnthropicEffortGoesInOutputConfig(t *testing.T) {
	body := map[string]any{}
	applyAnthropicThinking(body, ReasonHigh, 4096)

	config, ok := body["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("no output_config: body %v", body)
	}
	if config["effort"] != "high" {
		t.Errorf("effort = %v, want high", config["effort"])
	}
	if _, present := body["reasoning"]; present {
		t.Errorf("reasoning was sent; that field is ignored by this endpoint: %v", body)
	}
}

// Thinking off must send nothing at all, since it is billed differently when on.
func TestThinkingOffSendsNothing(t *testing.T) {
	for _, level := range []ReasonLevel{ReasonOff, ""} {
		body := map[string]any{}
		applyAnthropicThinking(body, level, 4096)
		if _, present := body["thinking"]; present {
			t.Errorf("level %q sent a thinking field: %v", level, body)
		}
		if _, present := body["output_config"]; present {
			t.Errorf("level %q sent an output_config: %v", level, body)
		}
	}
}

func TestAnthropicEffortMapping(t *testing.T) {
	cases := map[ReasonLevel]string{
		ReasonMinimal: "low",
		ReasonLow:     "medium",
		ReasonMedium:  "medium",
		ReasonHigh:    "high",
		ReasonXHigh:   "high",
		ReasonMax:     "high",
	}
	for level, want := range cases {
		if got := anthropicEffort(level); got != want {
			t.Errorf("anthropicEffort(%q) = %q, want %q", level, got, want)
		}
	}
}

// A budget at or above max_tokens is a 400, not a clamp, and a budget below the
// minimum is refused outright.
func TestThinkingBudgetIsBounded(t *testing.T) {
	body := map[string]any{}
	applyAnthropicThinkingBudget(body, 10, 4096)
	thinking, _ := body["thinking"].(map[string]any)
	if got := thinking["budget_tokens"]; got != minimumThinkingBudget {
		t.Errorf("a tiny budget became %v, want the minimum %d", got, minimumThinkingBudget)
	}

	body = map[string]any{}
	applyAnthropicThinkingBudget(body, 5000, 4096)
	thinking, _ = body["thinking"].(map[string]any)
	if got, ok := thinking["budget_tokens"].(int); !ok || got >= 4096 {
		t.Errorf("budget %v is not below max_tokens 4096, which is a 400", thinking["budget_tokens"])
	}

	// Too small a window leaves no room at all, so nothing is sent rather than
	// sending a request the endpoint will reject.
	body = map[string]any{}
	applyAnthropicThinkingBudget(body, 2048, 1024)
	if _, present := body["thinking"]; present {
		t.Errorf("a budget was sent for a window too small to hold one: %v", body)
	}
}

// The thinking fields have to survive into the actual request. Testing the helper
// directly left the wiring unobserved, which is the same mistake the stream
// assertions made earlier: green while the field never left the function.
func TestThinkingReachesTheWire(t *testing.T) {
	for _, level := range []ReasonLevel{ReasonHigh, ReasonOff} {
		var body []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
		}))
		client := &StreamingClient{
			cfg: ProviderConfig{
				Model: "claude-x", BaseURL: srv.URL,
				AnthropicNative: true, AnthropicTokens: stubTokens{token: "sk-test"},
			},
			http: srv.Client(),
		}
		err := client.streamAnthropic(context.Background(), StreamRequest{
			Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
			Reason:   level, MaxTokens: 4096,
		}, func(ProviderResponse) {})
		srv.Close()
		if err != nil {
			t.Fatalf("level %q: %v", level, err)
		}
		sent := string(body)
		hasThinking := strings.Contains(sent, "\"thinking\"")
		if level == ReasonOff && hasThinking {
			t.Errorf("thinking reached the wire with the level off: %s", sent)
		}
		if level == ReasonHigh && !hasThinking {
			t.Errorf("no thinking field reached the wire: %s", sent)
		}
		if level == ReasonHigh && !strings.Contains(sent, "output_config") {
			t.Errorf("no output_config reached the wire: %s", sent)
		}
	}
}

// The transport had no reachable path to a plain API key: it refused outright
// without a managed credential, so a normal Anthropic key could not use it even
// though the transport is what serves that endpoint.
func TestMessagesAcceptsAProfileAPIKey(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	client := &StreamingClient{
		cfg:  ProviderConfig{Model: "claude-x", BaseURL: srv.URL, AnthropicNative: true, APIKey: "sk-profile"},
		http: srv.Client(),
	}
	if err := client.streamAnthropic(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}}, MaxTokens: 64,
	}, func(ProviderResponse) {}); err != nil {
		t.Fatalf("a profile API key was refused: %v", err)
	}
	if got != "sk-profile" {
		t.Errorf("x-api-key = %q, want the profile key", got)
	}
}

// A managed credential wins over a profile key, because it is the one that renews.
func TestManagedCredentialWinsOverAProfileKey(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	client := &StreamingClient{
		cfg: ProviderConfig{
			Model: "claude-x", BaseURL: srv.URL, AnthropicNative: true,
			APIKey: "sk-profile", AnthropicTokens: stubTokens{token: "managed"},
		},
		http: srv.Client(),
	}
	if err := client.streamAnthropic(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}}, MaxTokens: 64,
	}, func(ProviderResponse) {}); err != nil {
		t.Fatal(err)
	}
	if got != "managed" {
		t.Errorf("x-api-key = %q, want the managed credential", got)
	}
}

// With neither, the error has to say both what is missing and what to do.
func TestMessagesWithNoCredentialSaysWhatToDo(t *testing.T) {
	client := &StreamingClient{cfg: ProviderConfig{Model: "claude-x", AnthropicNative: true}}
	_, err := client.anthropicToken(context.Background())
	if err == nil {
		t.Fatal("a request with no credential was allowed")
	}
	if !strings.Contains(err.Error(), "API key") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

// A Messages-compatible host must be detected from the profile, so a plain
// Anthropic or MiniMax key is not sent to chat completions.
func TestMessagesHostDetection(t *testing.T) {
	cases := map[string]bool{
		"https://api.anthropic.com":          true,
		"https://api.minimax.io/anthropic":   true,
		"https://api.minimaxi.com/anthropic": true,
		"https://api.openai.com/v1":          false,
		"https://api.apmix.ai/v1":            false,
		"https://api.deepseek.com":           false,
		"":                                   false,
	}
	for base, want := range cases {
		if got := UsesAnthropicMessages("m", base); got != want {
			t.Errorf("UsesAnthropicMessages(%q) = %v, want %v", base, got, want)
		}
	}
}

// The failure this guards was only visible once the tool result came back: the
// first request encoded fine, and the second carried a tool_use whose input was
// two JSON values, so json.Marshal rejected it with an error naming neither the
// tool nor the arguments. Both halves are checked here.
func TestToolInputIsOneJSONValueAndSurvivesEncoding(t *testing.T) {
	// A seed of {} followed by the streamed object, which is what the endpoint
	// sends for a tool_use block.
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"MiniMax-M3"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"bash","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n"

	var calls []StreamedToolCall
	client := &StreamingClient{}
	if err := client.readAnthropicStream(strings.NewReader(stream), func(r ProviderResponse) {
		calls = append(calls, r.ToolCalls...)
	}); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if calls[0].Arguments != `{"command":"ls"}` {
		t.Fatalf("arguments = %q, want the streamed object alone", calls[0].Arguments)
	}

	// The follow-up request: the tool_use and its result together. This is the
	// encode that failed in production.
	items := []Item{
		{Kind: "text", Role: RoleUser, Text: "list files"},
		{Kind: "tool_call", CallID: "c1", Name: "bash", Args: calls[0].Arguments},
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "bash", Output: "bin\nprompt"},
	}
	messages, err := encodeAnthropicMessages(items)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := json.Marshal(messages); err != nil {
		t.Fatalf("the follow-up request does not encode: %v", err)
	}
}

// A block that streams no fragments at all still has to produce a usable
// arguments object, from the seed or as {}.
func TestToolInputWithoutFragments(t *testing.T) {
	cases := []struct {
		name  string
		start string
		want  string
	}{
		{"empty object seed", `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"read","input":{}}}`, "{}"},
		{"no seed", `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"read"}}`, "{}"},
		{"seed carrying the object", `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"read","input":{"path":"/tmp/x"}}}`, `{"path":"/tmp/x"}`},
	}
	for _, tc := range cases {
		stream := strings.Join([]string{
			`data: {"type":"message_start","message":{"id":"msg_1","model":"m"}}`,
			tc.start,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_stop"}`,
		}, "\n\n") + "\n"
		var calls []StreamedToolCall
		client := &StreamingClient{}
		if err := client.readAnthropicStream(strings.NewReader(stream), func(r ProviderResponse) {
			calls = append(calls, r.ToolCalls...)
		}); err != nil {
			t.Fatalf("%s: stream: %v", tc.name, err)
		}
		if len(calls) != 1 {
			t.Fatalf("%s: got %d tool calls, want 1", tc.name, len(calls))
		}
		if calls[0].Arguments != tc.want {
			t.Errorf("%s: arguments = %q, want %q", tc.name, calls[0].Arguments, tc.want)
		}
		if !json.Valid([]byte(calls[0].Arguments)) {
			t.Errorf("%s: arguments %q are not one JSON value", tc.name, calls[0].Arguments)
		}
	}
}

// Malformed arguments are reported against the tool that produced them, with a
// bounded snippet, instead of surfacing as an encoding error from inside
// encoding/json that names neither.
func TestMalformedToolArgumentsAreNamedAndBounded(t *testing.T) {
	long := strings.Repeat("x", 400)
	_, err := encodeAnthropicMessages([]Item{
		{Kind: "tool_call", CallID: "c1", Name: "bash", Args: `{}{"command":"ls"}`},
	})
	if err == nil {
		t.Fatal("two concatenated JSON values were accepted as tool input")
	}
	if !strings.Contains(err.Error(), "bash") {
		t.Errorf("the error does not name the tool: %v", err)
	}

	_, err = encodeAnthropicMessages([]Item{
		{Kind: "tool_call", CallID: "c1", Name: "bash", Args: long},
	})
	if err == nil {
		t.Fatal("a non-JSON argument value was accepted")
	}
	if len(err.Error()) > 260 {
		t.Errorf("the error is %d bytes; the snippet is not bounded: %v", len(err.Error()), err)
	}
}

// Two hosts, two header styles, and each refuses the other's credential with a
// 401 naming the header it wanted. The agent backend answers an x-api-key request
// with {"code":401,"message":"token is required"}; the platform API answers a
// bearer token with a request for an API secret key. Getting this wrong is not a
// missing credential, it is a credential sent correctly to the wrong door.
func TestMessagesAuthStyleChoosesTheHeader(t *testing.T) {
	for _, tc := range []struct {
		style      string
		wantHeader string
		wantPrefix string
	}{
		{"", "x-api-key", ""},
		{"bearer", "Authorization", "Bearer "},
		{"Bearer", "Authorization", "Bearer "},
		{" BEARER ", "Authorization", "Bearer "},
	} {
		var seen http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = r.Header.Clone()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
		}))
		client := New(ProviderConfig{
			Model:             "m",
			BaseURL:           srv.URL,
			APIKey:            "secret-token",
			MessagesAuthStyle: tc.style,
		})
		err := client.streamAnthropic(context.Background(), StreamRequest{
			Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
		}, nil)
		srv.Close()
		if err != nil {
			t.Fatalf("style %q: %v", tc.style, err)
		}
		if got := seen.Get(tc.wantHeader); got != tc.wantPrefix+"secret-token" {
			t.Errorf("style %q: %s = %q, want %q", tc.style, tc.wantHeader, got, tc.wantPrefix+"secret-token")
		}
		// The other header must be absent, so a token is never offered twice and a
		// host that prefers one is never left to guess.
		other := "x-api-key"
		if tc.wantHeader == "x-api-key" {
			other = "Authorization"
		}
		if got := seen.Get(other); got != "" {
			t.Errorf("style %q: %s = %q, want it absent", tc.style, other, got)
		}
	}
}

// The agent backend's base already ends in the API version, and the Messages URL
// is built by appending to it. That is the shape that produces a doubled /v1 and
// a 404, so it is pinned here rather than discovered against the live host.
func TestTheAgentBackendBaseResolvesToOneMessagesURL(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"https://agent.minimax.io/mavis/api/v1/llm/v1", "https://agent.minimax.io/mavis/api/v1/llm/v1/messages"},
		{"https://agent.minimax.io/mavis/api/v1/llm/v1/", "https://agent.minimax.io/mavis/api/v1/llm/v1/messages"},
		// A host that already names the endpoint must not gain a second suffix.
		{"https://agent.minimax.io/mavis/api/v1/llm/v1/messages", "https://agent.minimax.io/mavis/api/v1/llm/v1/messages"},
		// The platform API, which is where an API secret key goes and which the
		// agent token is refused by.
		{"https://api.minimax.io/anthropic", "https://api.minimax.io/anthropic/v1/messages"},
	} {
		if got := anthropicMessagesURL(tc.base); got != tc.want {
			t.Errorf("anthropicMessagesURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

// Parallel tool calls arrive as one item, Kind "tool_calls", and the encoder had
// no arm for it. The whole turn fell through the switch and was dropped, so the
// tool_results that followed named tool_use ids the endpoint had never been sent:
//
//	invalid params, tool result's tool id(run_…_1_0) not found (2013)
//
// Every result must have a matching tool_use in the encoded request. That is the
// invariant, and it is what the endpoint checks.
func TestParallelToolCallsSurviveEncoding(t *testing.T) {
	items := []Item{
		{Kind: "text", Role: RoleUser, Text: "check three directories"},
		{Kind: "tool_calls", Text: "I'll look at all three.", ToolCalls: []ToolCall{
			{ID: "c0", Name: "bash", Args: `{"command":"ls /a"}`},
			{ID: "c1", Name: "bash", Args: `{"command":"ls /b"}`},
			{ID: "c2", Name: "bash", Args: `{"command":"ls /c"}`},
		}},
		{Kind: "text", Role: RoleTool, CallID: "c0", Name: "bash", Output: "a"},
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "bash", Output: "b"},
		{Kind: "text", Role: RoleTool, CallID: "c2", Name: "bash", Output: "c"},
	}
	messages, err := encodeAnthropicMessages(items)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	uses := map[string]bool{}
	results := map[string]bool{}
	for _, m := range messages {
		for _, block := range m.Content {
			switch block["type"] {
			case "tool_use":
				id, _ := block["id"].(string)
				uses[id] = true
			case "tool_result":
				id, _ := block["tool_use_id"].(string)
				results[id] = true
			}
		}
	}
	if len(uses) != 3 {
		t.Errorf("encoded %d tool_use blocks, want 3", len(uses))
	}
	for id := range results {
		if !uses[id] {
			t.Errorf("tool_result %s has no tool_use; this is the request the endpoint rejects", id)
		}
	}

	// One assistant turn, not one message per call: calls made together were
	// said together, and splitting them misrepresents the conversation.
	assistant := 0
	for _, m := range messages {
		if m.Role == "assistant" {
			assistant++
			if len(m.Content) != 4 {
				t.Errorf("assistant message has %d blocks, want text + 3 tool_use", len(m.Content))
			}
		}
	}
	if assistant != 1 {
		t.Errorf("%d assistant messages, want 1 for a single parallel turn", assistant)
	}
	if _, err := json.Marshal(messages); err != nil {
		t.Fatalf("the request does not encode: %v", err)
	}
}

// An empty batch must not produce an empty assistant turn, which the endpoint
// would reject outright.
func TestAnEmptyParallelBatchIsDropped(t *testing.T) {
	messages, err := encodeAnthropicMessages([]Item{
		{Kind: "text", Role: RoleUser, Text: "hi"},
		{Kind: "tool_calls"},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, m := range messages {
		if m.Role == "assistant" {
			t.Error("an empty tool_calls item produced an empty assistant message")
		}
	}
}

// A read of an image used to fall through to the generic reader, which returns
// string(bytes): a 400 KB JPEG is about a hundred thousand tokens of mojibake,
// spent silently before the model sees anything. The picture has to go over as an
// image block, and the path has to survive the session to get there.
func TestAnImageReadTravelsAsAnImageBlock(t *testing.T) {
	dir := t.TempDir()
	shot := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(shot, testPNG(t), 0o644); err != nil {
		t.Fatal(err)
	}

	items := []Item{
		{Kind: "text", Role: RoleUser, Text: "look at this"},
		{Kind: "tool_call", CallID: "c1", Name: "read", Args: `{"path":"shot.png"}`},
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "read",
			Output: "shot.png attached as an image (image/png, 1.2 kB).", Images: []string{shot}},
	}
	messages, err := encodeAnthropicMessages(items)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	encoded, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("the request does not encode: %v", err)
	}
	raw := string(encoded)

	if !strings.Contains(raw, `"type":"image"`) {
		t.Fatalf("no image block in the request:\n%s", raw)
	}
	if !strings.Contains(raw, `"media_type":"image/png"`) {
		t.Error("the image block has no media type")
	}
	// The payload is base64, never the file's bytes pasted into text.
	if strings.Contains(raw, "PNG") || strings.Contains(raw, "\x89PNG") {
		t.Error("raw PNG bytes reached the request as text")
	}
	if !strings.Contains(raw, "attached as an image") {
		t.Error("the result text describing the attachment is missing")
	}
}

// The session holds a path, never a copy of the picture, or every past message
// would carry the same image again.
func TestTheSessionCarriesAPathNotTheImage(t *testing.T) {
	if _, err := json.Marshal(Item{Images: []string{"/tmp/shot.png"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(Item{Images: []string{"/tmp/shot.png"}})
	if strings.Contains(string(raw), "base64") || strings.Contains(string(raw), "iVBOR") {
		t.Errorf("the item carries image bytes: %s", raw)
	}
	if !strings.Contains(string(raw), "/tmp/shot.png") {
		t.Errorf("the item does not carry the path: %s", raw)
	}
}

// A path that has since gone missing must not fail the turn: the result text
// already says what was attached, so the model still gets an answer.
func TestAMissingImageDegradesToText(t *testing.T) {
	blocks := anthropicImageBlocks([]string{filepath.Join(t.TempDir(), "gone.png")})
	if len(blocks) != 0 {
		t.Errorf("got %d blocks for a missing file, want none", len(blocks))
	}
	items := []Item{
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "read",
			Output: "shot.png attached as an image.", Images: []string{filepath.Join(t.TempDir(), "gone.png")}},
	}
	if _, err := encodeAnthropicMessages(items); err != nil {
		t.Errorf("a missing image failed the encode: %v", err)
	}
}

// Chat completions gets the same treatment, so reading a picture is not a
// property of one transport.
func TestChatEncoderSendsAnImageToo(t *testing.T) {
	dir := t.TempDir()
	shot := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(shot, testPNG(t), 0o644); err != nil {
		t.Fatal(err)
	}
	parts := chatImageParts([]string{shot})
	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1", len(parts))
	}
	encoded, _ := json.Marshal(parts[0])
	if !strings.Contains(string(encoded), "image_url") || !strings.Contains(string(encoded), "data:image/png;base64,") {
		t.Errorf("part = %s", encoded)
	}
}

// testPNG writes a small, valid PNG and returns its bytes. Decoders only are
// imported, so encoding goes through image/png in the test.
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 24, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 24; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 10), G: uint8(y * 10), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestUsageIsDeliveredOncePerStream is the assertion the other stream tests in
// this file structurally cannot make. They each keep the last usage they see, so
// a usage delivered twice and a usage delivered once are indistinguishable.
//
// The coordinator adds every delta it receives and counts the prompt locally
// once per request, so a stream that delivered usage on message_stop and again
// when the body ended reported each request's tokens twice. That is exactly 2.0x
// our own count, and because a real tokenizer counts more than the local len/4
// estimate it crossed the 2.0 agreement bound in ReconcileUsage and the run was
// marked untrusted. An untrusted run is not noisy: it reports no cost at all and
// a 0% cache read ratio, so both figures an operator uses to judge a provider
// went to zero. MiniMax runs on this transport.
func TestUsageIsDeliveredOncePerStream(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"MiniMax-M3"},"usage":{"input_tokens":120,"output_tokens":0}}`,
		`data: {"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":42}}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n"

	var deliveries int
	var summed UsageInfo
	client := &StreamingClient{}
	if err := client.readAnthropicStream(strings.NewReader(stream), func(r ProviderResponse) {
		if r.Usage != nil {
			deliveries++
			summed.Add(*r.Usage)
		}
	}); err != nil {
		t.Fatalf("stream: %v", err)
	}

	if deliveries != 1 {
		t.Errorf("usage delivered %d times, want 1: the coordinator sums every delta, so twice is 2.0x the local count", deliveries)
	}
	if summed.InputTokens != 120 || summed.OutputTokens != 42 {
		t.Errorf("summed usage = %d in / %d out, want 120 / 42", summed.InputTokens, summed.OutputTokens)
	}
}

// The end-of-body emit is the fallback for a stream that ends without
// message_stop, which a truncated-but-clean body does. Guarding the duplicate
// must not take that safety net with it.
func TestUsageStillArrivesWhenTheStreamHasNoMessageStop(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"MiniMax-M3"},"usage":{"input_tokens":120,"output_tokens":42}}`,
		`data: {"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
	}, "\n\n") + "\n"

	var deliveries int
	var last *UsageInfo
	client := &StreamingClient{}
	if err := client.readAnthropicStream(strings.NewReader(stream), func(r ProviderResponse) {
		if r.Usage != nil {
			deliveries++
			last = r.Usage
		}
	}); err != nil {
		t.Fatalf("stream: %v", err)
	}

	if deliveries != 1 {
		t.Errorf("usage delivered %d times, want 1", deliveries)
	}
	if last == nil || last.InputTokens != 120 {
		t.Errorf("terminal usage = %+v, want 120 input tokens", last)
	}
}

// refreshingTokens is a token source that can also renew, which is the capability
// the recovery path needs and the one plain stubTokens deliberately lacks.
type refreshingTokens struct {
	stubTokens
	mu        sync.Mutex
	refreshes int
	failWith  error
}

func (r *refreshingTokens) RefreshToken(context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshes++
	if r.failWith != nil {
		return "", r.failWith
	}
	return "fresh-token", nil
}

func (r *refreshingTokens) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refreshes
}

// A credential rejected before its recorded expiry is recoverable, so one forced
// exchange is attempted and the request replayed.
//
// This transport had no such path at all. TokenSource.RefreshToken has existed on
// the MiniMax credential the whole time and nothing here ever called it, because
// the only caller of the refresher interface lives in streamResponses. A token
// revoked server-side therefore burned three retries re-sending the same rejected
// credential and then failed, on a provider whose access token renews itself.
func TestARejectedCredentialIsRefreshedOnceAndReplayed(t *testing.T) {
	var mu sync.Mutex
	var authHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authHeaders = append(authHeaders, r.Header.Get("x-api-key"))
		attempt := len(authHeaders)
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"token expired"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	tokens := &refreshingTokens{stubTokens: stubTokens{token: "stale-token"}}
	client := &StreamingClient{
		cfg: ProviderConfig{
			Model: "MiniMax-M3", BaseURL: srv.URL,
			AnthropicNative: true, AnthropicTokens: tokens,
			MaxRetries: 2, RetryBackoff: time.Millisecond,
		},
		http: srv.Client(),
	}
	err := client.streamAnthropic(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}}, MaxTokens: 8,
	}, func(ProviderResponse) {})

	if err != nil {
		t.Fatalf("a recoverable rejection failed instead of replaying: %v", err)
	}
	if got := tokens.count(); got != 1 {
		t.Errorf("forced %d refresh(es), want exactly 1: a replay loop would hammer the token endpoint", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(authHeaders) != 2 {
		t.Fatalf("made %d request(s), want 2 (one rejected, one replayed)", len(authHeaders))
	}
	if authHeaders[1] != "fresh-token" {
		t.Errorf("replay sent %q, want the refreshed token", authHeaders[1])
	}
}

// One replay, not a loop. If the refreshed token is rejected too, the run must
// report the failure rather than exchanging credentials until the retries run
// out.
func TestTheRefreshIsNotRepeatedWhenTheNewTokenIsAlsoRejected(t *testing.T) {
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"still bad"}}`))
	}))
	defer srv.Close()

	tokens := &refreshingTokens{stubTokens: stubTokens{token: "stale-token"}}
	client := &StreamingClient{
		cfg: ProviderConfig{
			Model: "MiniMax-M3", BaseURL: srv.URL,
			AnthropicNative: true, AnthropicTokens: tokens,
			MaxRetries: 3, RetryBackoff: time.Millisecond,
		},
		http: srv.Client(),
	}
	err := client.streamAnthropic(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}}, MaxTokens: 8,
	}, func(ProviderResponse) {})

	if err == nil {
		t.Fatal("a credential rejected twice reported success")
	}
	if got := tokens.count(); got != 1 {
		t.Errorf("forced %d refresh(es), want 1", got)
	}
	// 401 is not retryable, so exactly two requests: the original and the one
	// replay. Three retries of a credential that cannot be renewed is the delay
	// this removed.
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("made %d request(s), want 2: a 401 is not retryable", calls)
	}
}

// A credential that cannot renew at all - a plain API key, or a provider whose
// token source has no refresher - must fail immediately rather than after three
// backoffs re-sending it. The Responses transport has always behaved this way.
func TestAnUnrecoverableRejectionIsNotRetried(t *testing.T) {
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid x-api-key"}}`))
	}))
	defer srv.Close()

	client := &StreamingClient{
		cfg: ProviderConfig{
			Model: "MiniMax-M3", BaseURL: srv.URL,
			AnthropicNative: true, AnthropicTokens: stubTokens{token: "wrong"},
			MaxRetries: 3, RetryBackoff: time.Millisecond,
		},
		http: srv.Client(),
	}
	err := client.streamAnthropic(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}}, MaxTokens: 8,
	}, func(ProviderResponse) {})

	if err == nil {
		t.Fatal("a rejected credential reported success")
	}
	var perr *ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("error is %T, want *ProviderError", err)
	}
	if perr.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", perr.Status)
	}
	if perr.Retryable {
		t.Error("a 401 is marked retryable, so a credential that cannot renew is re-sent with backoff")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("made %d request(s), want 1", calls)
	}
}

// The account-grant style: the DeepSeek account endpoint authenticates with
// x-dsh-auth-token, and neither of the other two headers is an authorized
// request there. Observed against a stub, like the x-api-key test above,
// because body assertions cannot see headers at all.
func TestMessagesRequestHeadersDshStyle(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	client := &StreamingClient{
		cfg: ProviderConfig{
			Model:             "deepseek-v4-pro",
			BaseURL:           srv.URL,
			AnthropicNative:   true,
			MessagesAuthStyle: MessagesAuthDsh,
			AnthropicTokens:   stubTokens{token: "TEST-DSH-GRANT"},
			MaxRetries:        0,
		},
		http: srv.Client(),
	}
	err := client.streamAnthropic(context.Background(), StreamRequest{
		Messages:  []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
		MaxTokens: 16,
	}, func(ProviderResponse) {})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got.Get("x-dsh-auth-token") != "TEST-DSH-GRANT" {
		t.Errorf("x-dsh-auth-token = %q, want the grant", got.Get("x-dsh-auth-token"))
	}
	if got.Get("x-api-key") != "" {
		t.Errorf("an x-api-key header was also sent: %q", got.Get("x-api-key"))
	}
	if got.Get("Authorization") != "" {
		t.Errorf("an Authorization header was also sent: %q", got.Get("Authorization"))
	}
}
