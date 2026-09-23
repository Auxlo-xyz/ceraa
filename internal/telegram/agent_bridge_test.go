package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"sooraa/harness/llm"
	"sooraa/harness/sessionstore"
	"sooraa/internal/config"
)

func TestRunAgentMaintainsToolPairsAcrossLongTurns(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode provider request: %v", err)
		}
		if requests.Load() > 0 {
			assertToolPairs(t, request.Messages)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		var payload map[string]any
		switch requests.Add(1) {
		case 1:
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": "call_1", "function": map[string]any{"name": "read", "arguments": `{"path":"../../go.mod"}`}},
				map[string]any{"index": 1, "id": "call_2", "function": map[string]any{"name": "read", "arguments": `{"path":"../../go.mod"}`}},
			}}}}}
		case 2:
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": "call_3", "function": map[string]any{"name": "read", "arguments": `{"path":"../../go.mod"}`}},
			}}}}}
		default:
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "finished"}}}}
		}
		encoded, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		LLM: config.LLMProvider{
			URL:             server.URL,
			APIKey:          "test",
			Model:           "test",
			ContextLimit:    236000,
			MaxOutputTokens: 1024,
		},
		MaxSteps:    5,
		SessionFile: dir,
	}
	var final string
	err := runAgentWithConfig("long-session", "work for several turns", cfg, nil, nil, func(answer string) { final = answer })
	if err != nil {
		t.Fatal(err)
	}
	if final != "finished" {
		t.Fatalf("unexpected final answer: %q", final)
	}
	if requests.Load() != 3 {
		t.Fatalf("expected 3 provider requests, got %d", requests.Load())
	}

	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Load("long-session")
	if err != nil {
		t.Fatal(err)
	}
	if sessionstore.Recover(sess) {
		t.Fatal("completed session still has unresolved operations")
	}
	calls := 0
	results := 0
	for _, message := range sess.Messages {
		calls += len(message.AllToolCalls())
		if message.Role == llm.RoleTool {
			results++
		}
	}
	if calls != 3 || results != 3 {
		t.Fatalf("expected 3 calls and 3 results, got %d and %d", calls, results)
	}
}

func assertToolPairs(t *testing.T, messages []map[string]any) {
	t.Helper()
	pending := make(map[string]bool)
	for _, message := range messages {
		role, _ := message["role"].(string)
		if role == "assistant" {
			calls, _ := message["tool_calls"].([]any)
			for _, raw := range calls {
				call, _ := raw.(map[string]any)
				id, _ := call["id"].(string)
				pending[id] = true
			}
		}
		if role != "tool" {
			continue
		}
		id, _ := message["tool_call_id"].(string)
		if !pending[id] {
			t.Errorf("tool result %q has no preceding assistant call", id)
		}
		delete(pending, id)
	}
	if len(pending) != 0 {
		t.Errorf("provider request contains unresolved tool calls: %v", pending)
	}
}

func TestRunAgentHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runAgentContext(ctx, "cancelled", "task", nil, nil, nil); err == nil {
		t.Fatal("expected cancellation error")
	}
}
