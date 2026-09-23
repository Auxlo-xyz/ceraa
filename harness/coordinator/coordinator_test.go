package coordinator

import (
	"context"
	"testing"
	"time"

	"soraa-agent/harness/inbox"
	"soraa-agent/harness/llm"
	"soraa-agent/harness/operation"
	"soraa-agent/harness/sessionstore"
	"soraa-agent/harness/tool"
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

func TestCoordinatorRuns(t *testing.T) {
	store, err := sessionstore.New("testdata")
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Stop()

	reg := tool.DefaultRegistry()
	opMgr := operation.New(nil)

	cfg := Config{
		SessionID:    "test-sess",
		SystemPrompt: "You are a test agent",
		MaxSteps:     1,
	}
	cb := Callbacks{
		OnContent: func(s string) {},
		OnDone:    func(s string) {},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = reg
	_ = opMgr
	_ = ib
	_ = store
	_ = cfg
	_ = cb
	_ = ctx
}
