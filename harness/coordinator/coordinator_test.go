package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ceraa/harness/inbox"
	"ceraa/harness/llm"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

func TestRunnerPromptCacheKeyIsStablePerProviderAndSession(t *testing.T) {
	first := &Runner{cfg: RunnerConfig{SessionID: "session-a", Model: llm.FromProvider("https://openrouter.ai/api/v1", "key", "model")}}
	second := &Runner{cfg: RunnerConfig{SessionID: "session-a", Model: llm.FromProvider("https://openrouter.ai/api/v1", "key", "model")}}
	otherSession := &Runner{cfg: RunnerConfig{SessionID: "session-b", Model: llm.FromProvider("https://openrouter.ai/api/v1", "key", "model")}}
	otherModel := &Runner{cfg: RunnerConfig{SessionID: "session-a", Model: llm.FromProvider("https://openrouter.ai/api/v1", "key", "other")}}
	if first.promptCacheKey() != second.promptCacheKey() {
		t.Fatal("cache key changed for the same session")
	}
	if first.promptCacheKey() == otherSession.promptCacheKey() || first.promptCacheKey() == otherModel.promptCacheKey() {
		t.Fatal("cache key did not include session or model")
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
