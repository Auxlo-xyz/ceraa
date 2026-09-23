package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"sooraa/harness/inbox"
	"sooraa/harness/llm"
	"sooraa/harness/operation"
	"sooraa/harness/sessionstore"
	"sooraa/harness/tool"
)

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
	err = RunAsync(ctx, Config{SessionID: "test-sess", SystemPrompt: "test", MaxSteps: 3}, store, tool.DefaultRegistry(), operation.New(nil), ib, Callbacks{OnDone: func(answer string) { done <- answer }})
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
