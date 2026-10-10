package qodersign

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The payload shape is the vendor client's own: every key below mirrors its
// construction, and the signer encrypts the whole body, so a missing or
// misnamed key fails at the server rather than locally.
func TestBuildPayloadCarriesTheRoutingFields(t *testing.T) {
	ask := Ask{
		ModelKey: "qfmodel",
		UserText: "hello",
		Business: map[string]any{"type": "agent"},
	}
	body := BuildPayload(ask, "req-1", "sess-1")
	if body["request_id"] != "req-1" || body["request_set_id"] != "req-1" || body["chat_record_id"] != "req-1" {
		t.Errorf("request id trio = %v %v %v, want all req-1", body["request_id"], body["request_set_id"], body["chat_record_id"])
	}
	if body["session_id"] != "sess-1" {
		t.Errorf("session_id = %v, want sess-1", body["session_id"])
	}
	if body["business"] == nil {
		t.Error("business is missing: without it the server routes to a dead node")
	}
	mc := body["model_config"].(map[string]any)
	if mc["key"] != "qfmodel" || mc["source"] != "system" || mc["format"] != "openai" || mc["is_vl"] != true || mc["max_input_tokens"] != 200000 {
		t.Errorf("model_config = %v, want key/source/format/is_vl/max_input_tokens defaults", mc)
	}
	if body["session_type"] != "qodercli" {
		t.Errorf("session_type = %v, want qodercli", body["session_type"])
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "hello" {
		t.Errorf("messages = %v, want the single user turn", msgs)
	}
	if tools, ok := body["tools"].([]any); !ok || len(tools) != 0 {
		t.Errorf("tools = %v, want an empty array rather than a missing key", body["tools"])
	}
	// The body must serialise: the signer encrypts its JSON, so anything here
	// that does not encode fails the whole request.
	if _, err := json.Marshal(body); err != nil {
		t.Errorf("payload does not encode: %v", err)
	}
}

// History travels verbatim but whitelisted: only the protocol's keys go
// upstream, and tool keys only where they belong.
func TestBuildPayloadKeepsToolKeysInPlace(t *testing.T) {
	call := ToolCall{ID: "c1", Type: "function"}
	call.Function.Name = "read"
	call.Function.Arguments = "{}"
	ask := Ask{
		ModelKey: "qfmodel",
		UserText: "go on",
		History: []Message{
			{Role: "user", Content: "read it"},
			{Role: "assistant", Content: "", ToolCalls: []ToolCall{call}},
			{Role: "tool", Content: "done", ToolCallID: "c1"},
		},
		Business: map[string]any{"type": "agent"},
	}
	body := BuildPayload(ask, "req-1", "sess-1")
	msgs := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want the 3 history turns", len(msgs))
	}
	assistant := msgs[1].(map[string]any)
	if _, ok := assistant["tool_calls"]; !ok {
		t.Error("assistant turn lost its tool_calls")
	}
	tool := msgs[2].(map[string]any)
	if tool["tool_call_id"] != "c1" {
		t.Errorf("tool turn = %v, want its tool_call_id", tool)
	}
	if _, ok := msgs[0].(map[string]any)["tool_calls"]; ok {
		t.Error("user turn gained tool_calls it never had")
	}
}

// Reasoning effort fans out to two parameters, and "none" disables thinking
// rather than requesting a level by that name.
func TestBuildPayloadReasoningEffort(t *testing.T) {
	ask := Ask{ModelKey: "qfmodel", UserText: "hi", ReasoningEffort: "high", Business: map[string]any{"type": "agent"}}
	params := BuildPayload(ask, "r", "s")["parameters"].(map[string]any)
	if params["reasoning_effort"] != "high" || params["enable_thinking"] != true {
		t.Errorf("parameters = %v, want effort high with thinking enabled", params)
	}
	ask.ReasoningEffort = "none"
	params = BuildPayload(ask, "r", "s")["parameters"].(map[string]any)
	if params["enable_thinking"] != false {
		t.Errorf("parameters = %v, want thinking disabled for none", params)
	}
}

// A signer without a uid is refused before touching WASM: every signature it
// could produce would be invalid, so attempting the call only wastes it.
func TestAuthFieldsRefusesAnEmptyUID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := New(ctx)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	defer s.Close(ctx)
	if _, _, err := s.AuthFields(UserInfo{UID: "", Token: "x"}); err == nil {
		t.Error("empty uid was accepted")
	} else if !strings.Contains(err.Error(), "uid") {
		t.Errorf("error = %q, want it to name the uid", err)
	}
}

// End to end against the real module with dummy identity: the fields are
// ciphertext, so this asserts shape and the COSY scheme marker rather than
// values. About 300ms; the module is small and the calls are pure crypto.
func TestSignerProducesACosyRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := New(ctx)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	defer s.Close(ctx)

	user := UserInfo{UID: "test-uid-123", Token: "test-token"}
	eui, key, err := s.AuthFields(user)
	if err != nil {
		t.Fatalf("auth fields: %v", err)
	}
	if eui == "" || key == "" {
		t.Fatal("empty auth fields")
	}
	c, err := s.NewContext("123e4567-e89b-12d3-a456-426614174000", ClientVersion, user,
		map[string]string{"encrypt_user_info": eui, "key": key}, DefaultClientMetadata())
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	ask := Ask{ModelKey: "qfmodel", UserText: "hello", Business: map[string]any{"type": "agent"}}
	payload, err := json.Marshal(BuildPayload(ask, newUUID(), newUUID()))
	if err != nil {
		t.Fatal(err)
	}
	req, err := c.PrepareInfer("https://api2.qoder.sh", string(payload), "qfmodel", "system")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	wantURL := "https://api2.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	if req.URL != wantURL {
		t.Errorf("url = %q, want %q", req.URL, wantURL)
	}
	auth := req.Headers["Authorization"]
	if !strings.HasPrefix(auth, "Bearer COSY.") {
		t.Errorf("authorization = %.40q, want a Bearer COSY signature", auth)
	}
	if req.Headers["Content-Type"] != "application/json" {
		t.Errorf("content-type = %q", req.Headers["Content-Type"])
	}
	if req.Body == "" {
		t.Error("empty encrypted body")
	}
}
