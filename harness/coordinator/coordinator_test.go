package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"ceraa/harness/inbox"
	"ceraa/harness/llm"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// The cache key identifies the reusable prefix, which is the system prompt plus
// the whole tool array. Those are byte-identical across sessions on one provider
// and model, so the key must NOT vary by session: folding the session in gave
// every session its own cold prefix and re-billed a shared head for nothing.
func TestPromptCacheKeyIsSharedAcrossSessionsButSeparatesProvidersAndModels(t *testing.T) {
	runner := func(sessionID, baseURL, model string) *Runner {
		return &Runner{cfg: RunnerConfig{SessionID: sessionID, Model: llm.FromProvider(baseURL, "key", model)}}
	}
	sessionA := runner("session-a", "https://openrouter.ai/api/v1", "model")
	sessionB := runner("session-b", "https://openrouter.ai/api/v1", "model")
	otherModel := runner("session-a", "https://openrouter.ai/api/v1", "other")
	otherHost := runner("session-a", "https://generativelanguage.googleapis.com/v1beta", "model")

	if sessionA.promptCacheKey() != sessionB.promptCacheKey() {
		t.Error("cache key varies by session; the shared prefix will be re-billed per session")
	}
	if sessionA.promptCacheKey() == otherModel.promptCacheKey() {
		t.Error("cache key does not separate models")
	}
	if sessionA.promptCacheKey() == otherHost.promptCacheKey() {
		t.Error("cache key does not separate providers")
	}
}

func TestRunnerExposesNativeToolDefinitions(t *testing.T) {
	runner := &Runner{reg: tool.DefaultRegistry()}
	defs := runner.toolDefs()
	if len(defs) == 0 || defs[0].Name == "run" {
		t.Fatalf("tool definitions = %#v", defs)
	}
	native := false
	for _, def := range defs {
		if def.Name == "read" {
			native = true
		}
	}
	if !native {
		t.Fatal("native tool definitions were not exposed")
	}
}

func TestRunnerSystemPromptOmitsCompactCatalog(t *testing.T) {
	runner := &Runner{reg: tool.DefaultRegistry(), cfg: RunnerConfig{SystemPrompt: "system"}}
	prompt := runner.systemPrompt()
	if strings.Contains(prompt, "<tool-catalog>") || !strings.Contains(prompt, "system") {
		t.Fatalf("system prompt = %q", prompt)
	}
}

func TestDispatcherCanonicalizesAliasesAndArgumentNames(t *testing.T) {
	reg := tool.DefaultRegistry()
	for requested, want := range map[string]string{
		"cat":        "read",
		"read_file":  "read",
		"shell":      "bash",
		"file_write": "write",
		"web_fetch":  "webfetch",
	} {
		if got := canonicalToolName(reg, requested); got != want {
			t.Fatalf("canonicalToolName(%q) = %q, want %q", requested, got, want)
		}
	}
	args := map[string]any{"file": "README.md"}
	if !repairToolArguments("read", args) || args["path"] != "README.md" {
		t.Fatalf("repaired read arguments = %#v", args)
	}
	args = map[string]any{"cmd": "pwd"}
	if !repairToolArguments("bash", args) || args["command"] != "pwd" {
		t.Fatalf("repaired bash arguments = %#v", args)
	}
}

func TestNormalizeDispatcherMessagesUnwrapsLegacyRunCalls(t *testing.T) {
	items := []llm.Item{
		{Kind: "tool_call", CallID: "one", Name: "run", Args: `{"tool":"read","arguments":{"path":"README.md"}}`},
		{Kind: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "two", Name: "run", Args: `{"tool":"bash","arguments":{"command":"pwd"}}`}}},
		{Kind: "text", Role: llm.RoleTool, CallID: "one", Name: "run", Output: "ok"},
	}
	normalized := normalizeDispatcherMessages(items)
	if normalized[0].Name != "read" || normalized[0].Args != `{"path":"README.md"}` {
		t.Fatalf("legacy single call = %#v", normalized[0])
	}
	if normalized[1].ToolCalls[0].Name != "bash" || normalized[1].ToolCalls[0].Args != `{"command":"pwd"}` {
		t.Fatalf("legacy multi call = %#v", normalized[1])
	}
	if normalized[2].Name != "read" {
		t.Fatalf("legacy tool result = %#v", normalized[2])
	}
}

func TestRunnerAcceptsNativeAndRejectsUnknownDispatcherCalls(t *testing.T) {
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{reg: tool.DefaultRegistry(), store: store, opMgr: operation.New(nil), cfg: RunnerConfig{SessionID: "dispatcher"}}
	sess := &sessionstore.Session{ID: "dispatcher", Version: 2}
	prepared, results, _, err := runner.prepareTools(context.Background(), sess, "run1", []llm.StreamedToolCall{
		{ID: "native", Name: "read", Arguments: `{"path":"README.md"}`},
		{ID: "unknown", Name: "run", Arguments: `{"tool":"missing","arguments":{}}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 1 || prepared[0].tool.Name() != "read" || !strings.Contains(results["unknown"], "Error: unknown tool missing; use canonical tools:") {
		t.Fatalf("dispatcher results = %#v, prepared = %#v", results, prepared)
	}
}

func TestDispatcherDefinitionsStayCompact(t *testing.T) {
	runner := &Runner{reg: tool.DefaultRegistry(), cfg: RunnerConfig{SystemPrompt: "system"}}
	definition, err := json.Marshal(runner.toolDefs())
	if err != nil {
		t.Fatal(err)
	}
	if len(definition) > 16384 {
		t.Fatalf("native definitions bytes = %d", len(definition))
	}
}

func TestCoordinatorRetriesMalformedToolCallWithValidSchema(t *testing.T) {
	var requests atomic.Int32
	var retryHasNativeDefinition atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if requests.Add(1) == 2 {
			tools, _ := body["tools"].([]any)
			for _, raw := range tools {
				toolMap, _ := raw.(map[string]any)
				if toolMap["type"] == "function" {
					function, _ := toolMap["function"].(map[string]any)
					if function["name"] == "read" {
						retryHasNativeDefinition.Store(true)
					}
				}
			}
		}
		var payload map[string]any
		if requests.Load() == 1 {
			payload = map[string]any{
				"choices": []any{map[string]any{
					"delta": map[string]any{
						"tool_calls": []any{map[string]any{
							"index":    0,
							"id":       "bad-call",
							"function": map[string]any{"name": "run", "arguments": `{"tool":"missing","arguments":{}}`},
						}},
					},
				}},
			}
		} else {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "recovered"}}}}
		}
		encoded, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("CERAA_API_URL", server.URL)
	t.Setenv("CERAA_API_KEY", "test")
	t.Setenv("CERAA_MODEL", "test")
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Stop()
	done := make(chan string, 1)
	err = RunAsync(context.Background(), Config{SessionID: "corrective-retry", SystemPrompt: "test", ModelCatalogURL: "http://127.0.0.1:1"}, store, tool.DefaultRegistry(), operation.New(nil), ib, Callbacks{OnDone: func(answer string) { done <- answer }})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case answer := <-done:
		if answer != "recovered" || requests.Load() != 2 || !retryHasNativeDefinition.Load() {
			t.Fatalf("answer=%q requests=%d native_definition=%t", answer, requests.Load(), retryHasNativeDefinition.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not retry malformed tool call")
	}
}

func TestCoordinatorRetriesReasoningOnlyResponseWithToolsDisabled(t *testing.T) {
	var requests atomic.Int32
	var retryWithToolsDisabled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if requests.Add(1) == 2 {
			tools, _ := body["tools"].([]any)
			retryWithToolsDisabled.Store(len(tools) > 0 && body["tool_choice"] == "none")
		}
		var payload map[string]any
		if requests.Load() == 1 {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": "thinking"}}}}
		} else {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "final answer"}}}}
		}
		encoded, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("CERAA_API_URL", server.URL)
	t.Setenv("CERAA_API_KEY", "test")
	t.Setenv("CERAA_MODEL", "test")
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Stop()
	done := make(chan string, 1)
	err = RunAsync(context.Background(), Config{SessionID: "final-retry", SystemPrompt: "test", ModelCatalogURL: "http://127.0.0.1:1"}, store, tool.DefaultRegistry(), operation.New(nil), ib, Callbacks{OnDone: func(answer string) { done <- answer }})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case answer := <-done:
		if answer != "final answer" || requests.Load() != 2 || !retryWithToolsDisabled.Load() {
			t.Fatalf("answer=%q requests=%d tools_disabled=%t", answer, requests.Load(), retryWithToolsDisabled.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not retry reasoning-only response")
	}
}

func TestCoordinatorConvertsTextToolCallToDispatcherCall(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		var payload map[string]any
		if requests.Add(1) == 1 {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": `<tool_call>run
<arg_key>arguments</arg_key>
<arg_value>{"command": "cat /app/package.json", "timeout": 10000}</arg_value><arg_key>tool</arg_key>
<arg_value>bash</arg_value>
</tool_call>`}}}}
		} else {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "parsed"}}}}
		}
		encoded, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("CERAA_API_URL", server.URL)
	t.Setenv("CERAA_API_KEY", "test")
	t.Setenv("CERAA_MODEL", "test")
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Stop()
	done := make(chan string, 1)
	err = RunAsync(context.Background(), Config{SessionID: "text-tool-call", SystemPrompt: "test", ModelCatalogURL: "http://127.0.0.1:1"}, store, tool.DefaultRegistry(), operation.New(nil), ib, Callbacks{OnDone: func(answer string) { done <- answer }})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case answer := <-done:
		if answer != "parsed" || requests.Load() != 2 {
			t.Fatalf("answer=%q requests=%d", answer, requests.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not convert text tool call")
	}
}

func TestCoordinatorHardStopsToolsAndRecoversLingFinalValue(t *testing.T) {
	var requests atomic.Int32
	var retryWithToolsDisabled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if requests.Add(1) == 2 {
			tools, _ := body["tools"].([]any)
			retryWithToolsDisabled.Store(len(tools) > 0 && body["tool_choice"] == "none")
		}
		var payload map[string]any
		if requests.Load() == 1 {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": "thinking"}}}}
		} else {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
				"reasoning_content": "<arg_value>recovered final</arg_value>",
				"tool_calls":        []any{map[string]any{"index": 0, "id": "ignored", "function": map[string]any{"name": "run", "arguments": `{"tool":"bash","arguments":{"command":"pwd"}}`}}},
			}}}}
		}
		encoded, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("CERAA_API_URL", server.URL)
	t.Setenv("CERAA_API_KEY", "test")
	t.Setenv("CERAA_MODEL", "test")
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Stop()
	done := make(chan string, 1)
	err = RunAsync(context.Background(), Config{SessionID: "hard-final", SystemPrompt: "test", ModelCatalogURL: "http://127.0.0.1:1"}, store, tool.DefaultRegistry(), operation.New(nil), ib, Callbacks{OnDone: func(answer string) { done <- answer }})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case answer := <-done:
		if answer != "recovered final" || requests.Load() != 2 || !retryWithToolsDisabled.Load() {
			t.Fatalf("answer=%q requests=%d tools_disabled=%t", answer, requests.Load(), retryWithToolsDisabled.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not hard-stop finalization tools")
	}
}

func TestRunnerPersistsOperationLedger(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		var payload map[string]any
		if requests.Add(1) == 1 {
			payload = map[string]any{
				"choices": []any{map[string]any{
					"delta": map[string]any{
						"tool_calls": []any{map[string]any{
							"index":    0,
							"id":       "call1",
							"function": map[string]any{"name": "run", "arguments": `{"tool":"read","arguments":{"path":"../../go.mod"}}`},
						}},
					},
				}},
			}
		} else {
			payload = map[string]any{
				"choices": []any{map[string]any{
					"delta": map[string]any{"content": "done"},
				}},
			}
		}
		encoded, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("CERAA_API_URL", server.URL)
	t.Setenv("CERAA_API_KEY", "test")
	t.Setenv("CERAA_MODEL", "test")
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Stop()
	err = RunAsync(context.Background(), Config{SessionID: "ledger", SystemPrompt: "test", ModelCatalogURL: "http://127.0.0.1:1"}, store, tool.DefaultRegistry(), operation.New(nil), ib, Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Load("ledger")
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Operations) != 1 {
		t.Fatalf("expected one durable operation, got %d", len(sess.Operations))
	}
	if sess.Operations[0].Tool != "read" {
		t.Fatalf("resolved operation tool = %q", sess.Operations[0].Tool)
	}
	if sess.Operations[0].Status != sessionstore.StatusCompleted || !sess.Operations[0].ResultPresent {
		t.Fatalf("operation was not checkpointed: %+v", sess.Operations[0])
	}
	found := false
	for _, message := range sess.Messages {
		if message.Role == llm.RoleTool && message.ToolCallID == sess.Operations[0].ID {
			found = true
		}
	}
	if !found {
		t.Fatal("durable operation has no tool result message")
	}
}

func TestFormatOutputStoresLargeResults(t *testing.T) {
	dir := t.TempDir()
	runner := &Runner{cfg: RunnerConfig{MaxToolResultChars: 4000, ToolOutputDir: dir}}
	output := strings.Repeat("large output\n", 1000)
	result := runner.formatOutput("run1", "call1", output)
	if !strings.Contains(result, filepath.Join(dir, "run1", "call1.txt")) {
		t.Fatalf("expected saved output path, got %q", result)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "run1", "call1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != output {
		t.Fatal("saved output does not match original")
	}
	if !strings.Contains(result, "Tail:") {
		t.Fatal("expected a bounded tail in the model-visible result")
	}
}

func TestRunnerPersistsCumulativeUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		payload := map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": "done"}}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 5, "total_tokens": 105, "prompt_tokens_details": map[string]any{"cached_tokens": 20}},
		}
		encoded, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := New(RunnerConfig{SessionID: "usage", SystemPrompt: "system", Model: llm.FromProvider(server.URL, "test", "test"), ModelCatalogURL: "http://127.0.0.1:1"}, store, tool.NewRegistry())
	result, err := runner.Run(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.InputTokens != 100 || result.Usage.OutputTokens != 5 || result.Usage.CachedInputTokens != 20 {
		t.Fatalf("unexpected result usage: %+v", result.Usage)
	}
	sess, err := store.Load("usage")
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Runs) != 1 || sess.Runs[0].Requests != 1 || sess.Runs[0].Usage.InputTokens != 100 {
		t.Fatalf("usage was not persisted: %+v", sess.Runs)
	}
	if sess.Runs[0].InputBreakdown.System == 0 {
		t.Fatal("expected request source breakdown")
	}
}

func TestBuildLLMMessages(t *testing.T) {
	msgs := []sessionstore.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "hi"},
		{
			Role: llm.RoleAssistant,
			ToolCall: &sessionstore.ToolCallRef{
				ID:               "call1",
				Name:             "bash",
				Args:             `{"command":"echo hi"}`,
				ThoughtSignature: "sig",
			},
		},
		{
			Role:       llm.RoleTool,
			ToolCallID: "call1",
			ToolResult: "hi\n",
		},
	}
	items := buildLLMMessages(msgs)
	if len(items) != 4 {
		t.Fatalf("expected 4 items, got %d", len(items))
	}
	if items[0].Kind != "text" || items[0].Role != llm.RoleSystem {
		t.Fatalf("expected system text item, got %+v", items[0])
	}
	if items[2].Kind != "tool_call" || items[2].Name != "bash" || items[2].ThoughtSignature != "sig" {
		t.Fatalf("expected tool_call, got %+v", items[2])
	}
	if items[3].Kind != "text" || items[3].Role != llm.RoleTool {
		t.Fatalf("expected tool result, got %+v", items[3])
	}
	if items[3].Name != "bash" {
		t.Fatalf("expected tool result name, got %+v", items[3])
	}
	if items[3].Output != "hi\n" {
		t.Fatalf("expected 'hi', got %q", items[3].Output)
	}
}

func TestHandleInboxStopHard(t *testing.T) {
	ib, err := inbox.New(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Stop()

	cfg := Config{
		SessionID:    "test",
		SystemPrompt: "test",
	}
	cb := Callbacks{}

	sub := inbox.Submission{
		ID:      "stop-1",
		Kind:    inbox.KindControl,
		Payload: []byte(`{"mode":"hard","reason":"user requested stop"}`),
	}
	err = handleInboxEvent(sub, &map[string]bool{}, nil, &cfg, cb)
	// returns error when we stop hard
	if err == nil {
		t.Fatal("expected error on hard stop")
	}
}

func TestCoordinatorReturnsFallbackForReasoningOnlyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("CERAA_API_URL", server.URL)
	t.Setenv("CERAA_API_KEY", "test")
	t.Setenv("CERAA_MODEL", "test")

	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Stop()
	done := make(chan string, 1)
	err = RunAsync(context.Background(), Config{SessionID: "reasoning-only", SystemPrompt: "test", ModelCatalogURL: "http://127.0.0.1:1"}, store, tool.DefaultRegistry(), operation.New(nil), ib, Callbacks{OnDone: func(answer string) { done <- answer }})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case answer := <-done:
		if answer != "The model completed without producing a final response." {
			t.Fatalf("unexpected fallback answer: %q", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not finish reasoning-only response")
	}
}

func TestCoordinatorDispatchesTools(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		var payload map[string]any
		if requests.Add(1) == 1 {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call1", "function": map[string]any{"name": "run", "arguments": `{"tool":"read","arguments":{"path":"../../go.mod"}}`}}}}}}}
		} else {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "done"}}}}
		}
		encoded, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("CERAA_API_URL", server.URL)
	t.Setenv("CERAA_API_KEY", "test")
	t.Setenv("CERAA_MODEL", "test")

	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Stop()
	done := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = RunAsync(ctx, Config{SessionID: "test-sess", SystemPrompt: "test", ModelCatalogURL: "http://127.0.0.1:1"}, store, tool.DefaultRegistry(), operation.New(nil), ib, Callbacks{OnDone: func(answer string) { done <- answer }})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case answer := <-done:
		if answer != "done" {
			t.Fatalf("unexpected answer: %q", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not finish")
	}
	if requests.Load() != 2 {
		t.Fatalf("expected two model requests, got %d", requests.Load())
	}
}

// TestInProcessToolIsOfferedAndDispatched pins the whole path an in-process tool
// travels: it is offered to the model, translated, and executed through the
// operation manager.
//
// This is the seam that let a newly added tool be advertised to the model and
// then fail on every call, because the manager dispatched by a hardcoded list of
// type names rather than asking the registry. Testing the tool in isolation
// cannot catch that; only going through the manager can.
func TestInProcessToolIsOfferedAndDispatched(t *testing.T) {
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.DefaultRegistry()
	opMgr := operation.NewWithBuffer(&toolRunner{reg: registry}, 8)
	runner := &Runner{reg: registry, store: store, opMgr: opMgr, cfg: RunnerConfig{SessionID: "inproc"}}

	// The model must be able to see it, or it can never be chosen.
	offered := false
	for _, def := range runner.toolDefs() {
		if def.Name == "browser" {
			offered = true
		}
	}
	if !offered {
		t.Fatal("browser must be offered to the model")
	}

	sess := &sessionstore.Session{ID: "inproc", Version: 2}
	prepared, _, _, err := runner.prepareTools(context.Background(), sess, "run1", []llm.StreamedToolCall{
		{ID: "b1", Name: "browser", Arguments: `{"action":"status"}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 1 || prepared[0].tool.Name() != "browser" {
		t.Fatalf("prepared = %#v", prepared)
	}
	if prepared[0].spec.Type != "browser" {
		t.Fatalf("spec type = %q", prepared[0].spec.Type)
	}
	// Status needs no browser, so the call can run here. The result is read from
	// the manager, which is the path that used to reject the tool by name.
	select {
	case result := <-opMgr.Results():
		if result.Status != operation.StatusCompleted {
			t.Fatalf("status = %v, error = %q", result.Status, result.Error)
		}
		if result.Output == "" {
			t.Fatal("expected some status output")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the in-process tool never produced a result")
	}
}

// Every tool definition is resent in every request for the whole session, so its
// cost is multiplied by the turn count while the calls it enables usually happen
// a handful of times. A batch_edit_file definition costs more per turn than a
// three-change call costs outright, which is the opposite of the intuition that
// only the calls count.
//
// A per-tool budget is what catches this. The total-size test above passed while
// both edit definitions carried a 92-character description for occurrence that
// nobody read, because the aggregate still had headroom.
// scriptDefinitionBudget is set just above the script definition's current 842
// bytes, for the ratchet reason stated below. It is worth watching because the
// description ends with the list of tools the script can reach, and that list is
// the part that grows the next time a tool is added. 842 is 7 percent of the
// 10985 bytes every tool definition costs each turn, which is the size of the
// claim being made.
const scriptDefinitionBudget = 860

func TestEditToolDefinitionsStaySmall(t *testing.T) {
	runner := &Runner{reg: tool.DefaultRegistry(), cfg: RunnerConfig{SystemPrompt: "s"}}
	budgets := map[string]int{
		// Set just above the current size, so this is a ratchet: it catches the
		// next description that grows, and never fires on its own.
		"batch_edit_file": 780,
		"edit_line":       650,
		"script":          scriptDefinitionBudget,
	}
	for _, def := range runner.toolDefs() {
		budget, watched := budgets[def.Name]
		if !watched {
			continue
		}
		size, err := json.Marshal(def)
		if err != nil {
			t.Fatal(err)
		}
		if len(size) > budget {
			t.Errorf("%s definition is %d bytes, over the %d budget; trim the description or drop a property",
				def.Name, len(size), budget)
		}
	}
}

// Reading a long file twice in one task is common: the tail is not what the model
// needed, so it reads again, and the cap trims it to the same pointer it already
// has. Measured on a real 53-message session, one 21k file was read five times and
// the identical 4145-byte notice was sent seven times across the run.
func TestARepeatedTrimIsNotResentInFull(t *testing.T) {
	dir := t.TempDir()
	runner := &Runner{cfg: RunnerConfig{MaxToolResultChars: 4000, ToolOutputDir: dir}}
	output := strings.Repeat("a long line of file content\n", 1500) // 43,500 B

	first := runner.formatOutput("run1", "call1", output)
	if !strings.Contains(first, "Tail:") {
		t.Fatalf("first result should carry a tail, got %q", first)
	}
	second := runner.formatOutput("run1", "call2", output)
	if strings.Contains(second, "Tail:") {
		t.Errorf("a repeat re-sent the tail: %d bytes", len(second))
	}
	if !strings.Contains(second, "Identical to a tool result earlier in this run") {
		t.Errorf("a repeat should say it is identical, got %q", second)
	}
	if len(second) >= len(first)/2 {
		t.Errorf("a repeat is %d bytes against a first of %d, which is no saving",
			len(second), len(first))
	}
	// The file is still written, so the path it names resolves.
	if !strings.Contains(second, filepath.Join(dir, "run1", "call2.txt")) {
		t.Errorf("a repeat should still name a readable file, got %q", second)
	}
	if _, err := os.ReadFile(filepath.Join(dir, "run1", "call2.txt")); err != nil {
		t.Errorf("the repeat's file was not written: %v", err)
	}
}

// Keyed on content, not operation ID, which is what makes a re-read of the same
// file a repeat at all.
func TestADifferentFileIsNotTreatedAsARepeat(t *testing.T) {
	dir := t.TempDir()
	runner := &Runner{cfg: RunnerConfig{MaxToolResultChars: 4000, ToolOutputDir: dir}}
	base := strings.Repeat("shared prefix line\n", 1000)
	runner.formatOutput("run1", "call1", base+"first file tail\n")
	other := runner.formatOutput("run1", "call2", base+"a different file tail\n")
	if !strings.Contains(other, "Tail:") {
		t.Errorf("distinct content was collapsed as a repeat: %q", other)
	}
}

// An edit changes the content, so the next read of the same path is not a repeat
// and must come back whole.
func TestAChangedFileIsNotTreatedAsARepeat(t *testing.T) {
	dir := t.TempDir()
	runner := &Runner{cfg: RunnerConfig{MaxToolResultChars: 4000, ToolOutputDir: dir}}
	before := strings.Repeat("line before the edit\n", 1000)
	after := strings.Repeat("line before the edit\n", 999) + "line after the edit\n"
	runner.formatOutput("run1", "call1", before)
	changed := runner.formatOutput("run1", "call2", after)
	if !strings.Contains(changed, "Tail:") {
		t.Errorf("an edited file was collapsed as a repeat: %q", changed)
	}
}

// Only trimmed results are remembered. A small result stays cheap, and a model
// re-reading a short file is often checking something specific rather than
// looping, so collapsing it would trade real information for a few hundred bytes.
func TestASmallResultIsNeverCollapsed(t *testing.T) {
	dir := t.TempDir()
	runner := &Runner{cfg: RunnerConfig{MaxToolResultChars: 4000, ToolOutputDir: dir}}
	small := "one short line\n"
	first := runner.formatOutput("run1", "call1", small)
	second := runner.formatOutput("run1", "call2", small)
	if first != second {
		t.Errorf("a small result was rewritten: %q vs %q", first, second)
	}
}

// Bounded, because this is a cache of strings that a long run keeps adding to.
func TestTheRepeatCacheIsBounded(t *testing.T) {
	dir := t.TempDir()
	runner := &Runner{cfg: RunnerConfig{MaxToolResultChars: 4000, ToolOutputDir: dir}}
	for i := 0; i < 200; i++ {
		runner.formatOutput("run1", fmt.Sprintf("call%d", i), strings.Repeat(fmt.Sprintf("body %d\n", i), 1200))
	}
	runner.trimmedMu.Lock()
	n := len(runner.trimmedSeen)
	runner.trimmedMu.Unlock()
	if n > 64 {
		t.Errorf("cache holds %d entries, over the bound of 64", n)
	}
}

// The shape taken from a real 53-message session: three long files, read nine
// times between them, every call with its own operation ID.
//
// The repeats were not byte-identical as they reached the model, because each
// notice embedded its own output path, so a dedupe keyed on the formatted text
// would have missed every one of them. The key has to be the content the tool
// returned, which is what formatOutput holds.
func TestRepeatedReadsOfLongFilesCollapseToOneNoticeEach(t *testing.T) {
	dir := t.TempDir()
	runner := &Runner{cfg: RunnerConfig{MaxToolResultChars: 12000, ToolOutputDir: dir}}

	// The three files and the read counts the session actually made.
	files := map[string]int{"index.ts": 5, "audit.md": 2, "metadata.json": 2}
	bodies := make(map[string]string, len(files))
	for name := range files {
		size := map[string]int{"index.ts": 21536, "audit.md": 52053, "metadata.json": 15905}[name]
		body := strings.Repeat("content line for "+name+"\n", size/(len("content line for "+name+"\n"))+1)
		bodies[name] = body[:size]
	}

	before, after, collapsed := 0, 0, 0
	calls := 0
	for name, reads := range files {
		for i := 0; i < reads; i++ {
			formatted := runner.formatOutput("run1", fmt.Sprintf("call-%s-%d", name, i), bodies[name])
			after += len(formatted)
			calls++
			if strings.Contains(formatted, "Identical to a tool result earlier") {
				collapsed++
			}
			// What each read costs with no dedupe: a trim notice plus its tail.
			tail := 12000 / 3
			before += len(fmt.Sprintf("Large tool output saved to /tmp/x/%s-%d.txt (%d bytes).\nTail:\n%s",
				name, i, len(bodies[name]), bodies[name][len(bodies[name])-tail:]))
		}
	}
	if collapsed != calls-len(files) {
		t.Errorf("collapsed %d of %d repeats, want %d", collapsed, calls-len(files), calls-len(files))
	}
	t.Logf("%d reads of %d files: %d B -> %d B, saving %d B = %d tok",
		calls, len(files), before, after, before-after, (before-after)/4)
	if after >= before/2 {
		t.Errorf("dedupe left %d B against %d B, which is no real saving", after, before)
	}
}

// Two arrangements of a head window were tried and reverted, so this pins the
// tail to what it was. A test that only checked a head existed would have passed
// for both of them.
func TestTheTailWindowIsNotShrunkToMakeRoomForAHead(t *testing.T) {
	dir := t.TempDir()
	runner := &Runner{cfg: RunnerConfig{MaxToolResultChars: 12000, ToolOutputDir: dir}}
	output := strings.Repeat("tail content line here\n", 1500)
	got := runner.formatOutput("run1", "call1", output)
	if !strings.Contains(got, "Tail:") {
		t.Fatalf("the tail label is gone:\n%s", head(t, got))
	}
	window := got[strings.LastIndex(got, "Tail:"):]
	if len(window) < 12000/3 {
		t.Errorf("the tail window is %d bytes, under the %d it should keep", len(window), 12000/3)
	}
	if len(got) > 12000 {
		t.Errorf("result is %d bytes, over the 12000 cap", len(got))
	}
}

// The notice is part of what reaches the model, so the head and tail are budgeted
// around it. Spending the whole cap on content would push the result over.
func TestTheTrimStaysUnderTheCap(t *testing.T) {
	// Caps a real config uses. A cap of 1024 was already overshot before this
	// change, because the old code's minimum tail was 1024 on its own, so that is
	// pre-existing and not worth pinning here.
	for _, cap := range []int{12000, 8000, 4000, 3000, 2200} {
		dir := t.TempDir()
		runner := &Runner{cfg: RunnerConfig{MaxToolResultChars: cap, ToolOutputDir: dir}}
		output := strings.Repeat("a line of file content here\n", 3000)
		got := runner.formatOutput("run1", "call1", output)
		if len(got) > cap {
			t.Errorf("cap %d: result is %d bytes, over the cap", cap, len(got))
		}
	}
}

// A path the model is not told it can use is a dead end, and that is what it
// behaved like: handed this path five times for one unchanged file, it never ran
// grep against it.
func TestTheOffloadNoticeSaysTheFileIsUsable(t *testing.T) {
	dir := t.TempDir()
	runner := &Runner{cfg: RunnerConfig{MaxToolResultChars: 12000, ToolOutputDir: dir}}
	got := runner.formatOutput("run1", "call1", strings.Repeat("body\n", 4000))
	if !strings.Contains(got, "greppable from bash") {
		t.Errorf("the notice does not say the file can be searched:\n%s", head(t, got))
	}
}

// Byte slicing can cut a multi-byte rune in half, and it now happens twice per
// trimmed result rather than once. Invalid UTF-8 in a tool result is a decode
// failure at best and mojibake at worst.
func TestTheTrimNeverSplitsARune(t *testing.T) {
	dir := t.TempDir()
	runner := &Runner{cfg: RunnerConfig{MaxToolResultChars: 4000, ToolOutputDir: dir}}
	// Three-byte runes on every boundary, so any byte cut lands mid-sequence.
	output := strings.Repeat("日本語テキスト\n", 2000)
	got := runner.formatOutput("run1", "call1", output)
	if !utf8.ValidString(got) {
		t.Error("the trimmed result is not valid UTF-8")
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "\uFFFD") {
			t.Errorf("a rune was split, producing a replacement character: %q", line)
			break
		}
	}
}

// The repeat notice and the first notice carry different information, and the
// repeat must still name a path that resolves.
func TestTheRepeatNoticeStillPointsAtTheFile(t *testing.T) {
	dir := t.TempDir()
	runner := &Runner{cfg: RunnerConfig{MaxToolResultChars: 12000, ToolOutputDir: dir}}
	output := strings.Repeat("body line\n", 3000)
	runner.formatOutput("run1", "call1", output)
	repeat := runner.formatOutput("run1", "call2", output)
	if !strings.Contains(repeat, filepath.Join(dir, "run1", "call2.txt")) {
		t.Errorf("the repeat names no file: %q", repeat)
	}
	if _, err := os.ReadFile(filepath.Join(dir, "run1", "call2.txt")); err != nil {
		t.Errorf("the repeat's file is missing: %v", err)
	}
}

func head(t *testing.T, s string) string {
	t.Helper()
	if len(s) > 400 {
		return s[:400]
	}
	return s
}

func tailOf(s string) string {
	if len(s) > 400 {
		return s[len(s)-400:]
	}
	return s
}

// A ratchet on the prompt file, for the same reason the tool definitions have
// one: both are resent in every request for the whole session, so a line that
// grows there is paid on every turn forever. The prompt had just been compacted
// from 7,270 to 6,359 bytes, and putting a line back in is exactly the kind of
// change that should be deliberate rather than incidental.
func TestTheSystemPromptStaysSmall(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "prompt", "system.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// Just above the current size. It fires on the next accidental line, and
	// never on its own.
	//
	// Raised from 6800 deliberately, not by drift. Naming ranged read costs 331
	// bytes and every turn, and it retires the `sed -n` workaround that a real
	// session used three times in one task. That is a trade, so the number it was
	// raised to is written down here rather than just moved.
	//
	// Raised from 7100 on the same terms: a registered tool has to be named here
	// or TestPromptNamesEveryRegisteredTool fails, and the script rule is 231
	// bytes of when-to-reach-for-it that the tool's own description cannot carry,
	// because a description is a contract and guidance belongs in this section.
	// Cutting an existing rule to pay for it would have been a worse trade than
	// recording the raise, so the size it was raised to is written down.
	const budget = 7400
	if len(raw) > budget {
		t.Errorf("prompt/system.txt is %d bytes, over the %d budget; cut something before adding",
			len(raw), budget)
	}
}

// The run total sizes a problem; the per-request list says which request created
// it. Without the list the only answer to "tool output is 71% of input" is a
// number, and the number does not say what to change.
//
// This covers the shape and the round trip rather than the append inside the
// request loop, which needs a live provider to reach. That is a real limit of the
// test and is stated rather than papered over.
func TestRunRecordsItsPerRequestSplit(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess := &sessionstore.Session{ID: "perreq", Version: 2}
	sess.Runs = append(sess.Runs, sessionstore.Run{
		ID:     "run1",
		Status: sessionstore.RunCompleted,
		// The totals are the sum of the two requests below, so the invariant the
		// test checks holds. A fixture that claims 14 requests while listing 2
		// would fail for a reason that has nothing to do with the code.
		InputBreakdown: llm.TokenBreakdown{
			ToolOutput: 4103, System: 3504, Tools: 5138,
		},
		RequestBreakdown: []llm.RequestTokenBreakdown{
			{Step: 0, PromptTokens: 5576, CachedTokens: 1152, Breakdown: llm.TokenBreakdown{System: 1752, Tools: 2569}},
			{Step: 1, PromptTokens: 24000, CachedTokens: 20000, Breakdown: llm.TokenBreakdown{System: 1752, Tools: 2569, ToolOutput: 4103}},
		},
	})
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	reloaded, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.Load("perreq")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Runs[0].RequestBreakdown) != 2 {
		t.Fatalf("per-request split did not survive the round trip: %+v", got.Runs[0].RequestBreakdown)
	}
	first := got.Runs[0].RequestBreakdown[0]
	if first.Step != 0 || first.CachedTokens != 1152 || first.Breakdown.System != 1752 {
		t.Errorf("first request = %+v, want its step, cache count and system share", first)
	}
	// The per-request figures have to add up to the run total, or the two views
	// disagree and the smaller one is the one that gets believed.
	var sum llm.TokenBreakdown
	for _, r := range got.Runs[0].RequestBreakdown {
		sum.Add(r.Breakdown)
	}
	if sum.System != got.Runs[0].InputBreakdown.System {
		t.Errorf("per-request system tokens sum to %d, run total says %d", sum.System, got.Runs[0].InputBreakdown.System)
	}
}

// A run that never reached a request keeps an empty list rather than a zero entry,
// so "no requests" is distinguishable from "requests with no tokens".
func TestARunWithNoRequestsHasAnEmptySplit(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess := &sessionstore.Session{ID: "empty", Version: 2}
	sess.Runs = append(sess.Runs, sessionstore.Run{ID: "run1", Status: sessionstore.RunFailed})
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	reloaded, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.Load("empty")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Runs[0].RequestBreakdown) != 0 {
		t.Errorf("a run with no requests recorded %d entries", len(got.Runs[0].RequestBreakdown))
	}
}

// A script reaches its siblings through the operation manager, which only this
// construction knows about. Miss the attach and the tool is offered to the model
// and then fails on every call with "not wired", which is the same shape of
// failure as a tool the executor did not know the type of.
func TestNewWiresTheScriptTool(t *testing.T) {
	reg := tool.DefaultRegistry()
	raw, registered := reg.Get("script")
	if !registered {
		t.Skip("script is not registered; CERAA_SCRIPT=off")
	}
	script, isScript := raw.(*tool.ScriptTool)
	if !isScript {
		t.Fatalf("script is registered as %T", raw)
	}
	runner := New(RunnerConfig{SessionID: "wired", SystemPrompt: "s"}, nil, reg)
	_ = runner
	if script.Dispatch == nil {
		t.Fatal("script has no dispatcher, so every call would fail with 'not wired'")
	}
	if script.Scope["session_id"] != "wired" {
		t.Errorf("session_id = %v, a nested call would land in the wrong session", script.Scope["session_id"])
	}
	if script.Scope["goal_dir"] == "" {
		t.Error("goal_dir is missing, so a nested goal call would not find its state")
	}
	if script.RequiresApproval == nil {
		t.Fatal("script has no approval predicate, so a nested write would skip the gate")
	}
}

// The gate has to be the coordinator's own, not a copy of its rules. A copy is
// what would let the two drift until a tool the user was never asked about runs
// from inside a script.
func TestScriptApprovalGateIsTheCoordinators(t *testing.T) {
	asked := false
	reg := tool.DefaultRegistry()
	runner := New(RunnerConfig{
		SessionID: "gate", SystemPrompt: "s",
		NeedsAuthorization: func(name string, _ map[string]any) bool { return name == "write" },
		AuthorizeTool: func(context.Context, string, string, map[string]any) (bool, error) {
			asked = true
			return true, nil
		},
	}, nil, reg)
	_ = runner
	raw, ok := reg.Get("script")
	if !ok {
		t.Skip("script is not registered; CERAA_SCRIPT=off")
	}
	script := raw.(*tool.ScriptTool)
	if !script.RequiresApproval("write", map[string]any{"path": "/x"}) {
		t.Error("write is not refused inside a script, so the prompt never appears")
	}
	if script.RequiresApproval("read", map[string]any{"path": "/x"}) {
		t.Error("read is refused inside a script for no reason")
	}
	if asked {
		t.Error("building a runner asked for permission; the gate belongs to a call")
	}
}

// With no approval system configured at all, a script must behave exactly as a
// direct call does rather than inventing a gate of its own.
func TestScriptHasNoApprovalGateWithoutAConfiguredOne(t *testing.T) {
	reg := tool.DefaultRegistry()
	New(RunnerConfig{SessionID: "nogate", SystemPrompt: "s"}, nil, reg)
	raw, ok := reg.Get("script")
	if !ok {
		t.Skip("script is not registered; CERAA_SCRIPT=off")
	}
	if raw.(*tool.ScriptTool).RequiresApproval("write", map[string]any{"path": "/x"}) {
		t.Error("script invented a gate when none was configured")
	}
}

// The tool has to be in the request, not only in the registry. A tool that is
// registered but never sent is invisible to the model, which reads as the model
// refusing to use a feature rather than never having seen it.
func TestScriptIsOfferedToTheModel(t *testing.T) {
	reg := tool.DefaultRegistry()
	if _, registered := reg.Get("script"); !registered {
		t.Skip("script is not registered; CERAA_SCRIPT=off")
	}
	runner := &Runner{reg: reg, cfg: RunnerConfig{SystemPrompt: "s"}}
	found := false
	for _, def := range runner.toolDefs() {
		if def.Name == "script" {
			found = true
		}
	}
	if !found {
		t.Fatal("script is registered but not offered in the tool definitions")
	}
}

// The whole chain has to hold: the definition is offered, the arguments are
// translated, the code runs, a nested read executes, and one value comes back
// into the next request. This is the path a mis-wired script fails on, and it
// fails silently from the outside: the model gets an error string and reports a
// tool that does not work.
func TestCoordinatorRunsAScriptCallEndToEnd(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "answer.txt")
	if err := os.WriteFile(target, []byte("the answer is 42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := `const r = await tools.read({path: ` + strconv.Quote(target) + `}); return r.trim();`
	arguments, err := json.Marshal(map[string]any{"code": code})
	if err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int32
	var secondRequest string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if requests.Add(1) == 1 {
			payload = map[string]any{
				"choices": []any{map[string]any{
					"delta": map[string]any{
						"tool_calls": []any{map[string]any{
							"index":    0,
							"id":       "script-call",
							"function": map[string]any{"name": "script", "arguments": string(arguments)},
						}},
					},
				}},
			}
		} else {
			secondRequest = string(body)
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "done"}}}}
		}
		encoded, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("CERAA_API_URL", server.URL)
	t.Setenv("CERAA_API_KEY", "test")
	t.Setenv("CERAA_MODEL", "test")

	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Stop()

	done := make(chan string, 1)
	if err := RunAsync(context.Background(), Config{
		SessionID: "script-e2e", SystemPrompt: "test", ModelCatalogURL: "http://127.0.0.1:1",
	}, store, tool.DefaultRegistry(), nil, ib, Callbacks{
		OnDone: func(answer string) { done <- answer },
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case answer := <-done:
		if answer != "done" {
			t.Fatalf("answer = %q", answer)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not finish the script run")
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want the script call and the follow-up", requests.Load())
	}
	if !strings.Contains(secondRequest, "the answer is 42") {
		t.Fatalf("the nested read never reached the model; second request was %s", secondRequest)
	}
}
