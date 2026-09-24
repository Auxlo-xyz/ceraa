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

	"sooraa/harness/inbox"
	"sooraa/harness/llm"
	"sooraa/harness/operation"
	"sooraa/harness/sessionstore"
	"sooraa/harness/tool"
)

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
							"function": map[string]any{"name": "read", "arguments": `{"path":"../../go.mod"}`},
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
	t.Setenv("SOORAA_API_URL", server.URL)
	t.Setenv("SOORAA_API_KEY", "test")
	t.Setenv("SOORAA_MODEL", "test")
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Stop()
	err = RunAsync(context.Background(), Config{SessionID: "ledger", SystemPrompt: "test", MaxSteps: 3, ModelCatalogURL: "http://127.0.0.1:1"}, store, tool.DefaultRegistry(), operation.New(nil), ib, Callbacks{})
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
	runner := New(RunnerConfig{SessionID: "usage", SystemPrompt: "system", MaxSteps: 1, Model: llm.FromProvider(server.URL, "test", "test"), ModelCatalogURL: "http://127.0.0.1:1"}, store, tool.NewRegistry())
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
				ID:   "call1",
				Name: "bash",
				Args: `{"command":"echo hi"}`,
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
	if items[2].Kind != "tool_call" || items[2].Name != "bash" {
		t.Fatalf("expected tool_call, got %+v", items[2])
	}
	if items[3].Kind != "text" || items[3].Role != llm.RoleTool {
		t.Fatalf("expected tool result, got %+v", items[3])
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
		MaxSteps:     1,
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

func TestCoordinatorDispatchesTools(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		var payload map[string]any
		if requests.Add(1) == 1 {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call1", "function": map[string]any{"name": "read", "arguments": `{"path":"../../go.mod"}`}}}}}}}
		} else {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "done"}}}}
		}
		encoded, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("SOORAA_API_URL", server.URL)
	t.Setenv("SOORAA_API_KEY", "test")
	t.Setenv("SOORAA_MODEL", "test")

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
	err = RunAsync(ctx, Config{SessionID: "test-sess", SystemPrompt: "test", MaxSteps: 3, ModelCatalogURL: "http://127.0.0.1:1"}, store, tool.DefaultRegistry(), operation.New(nil), ib, Callbacks{OnDone: func(answer string) { done <- answer }})
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
