package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ceraa/harness/qodersign"
)

type stubQoderCreds struct {
	uid, token, machine string
	refreshed           *int32
}

func (s stubQoderCreds) QoderUID(context.Context) (string, error) { return s.uid, nil }
func (s stubQoderCreds) QoderToken(context.Context) (string, error) {
	return s.token, nil
}
func (s stubQoderCreds) QoderMachineID(context.Context) (string, error) {
	return s.machine, nil
}
func (s stubQoderCreds) RefreshQoderToken(context.Context) (string, error) {
	atomic.AddInt32(s.refreshed, 1)
	return s.token, nil
}

func testQoderCreds() stubQoderCreds {
	var n int32
	return stubQoderCreds{uid: "u-test", token: "tok-test", machine: "mid-test", refreshed: &n}
}

// Display names route on directory keys, and a bare key passes through for
// hand configuration. An unknown name must fail before signing, not at the
// server.
func TestQoderModelMapping(t *testing.T) {
	for display, key := range map[string]string{
		"Qwen3.8-Flash":  "qfmodel",
		"Qwen3.8-Max":    "qmodel_38max",
		"Auto":           "auto",
		"DeepSeek-Flash": "dfmodel",
		"MiniMax-M3":     "mmodel",
		"qfmodel":        "qfmodel",
	} {
		info, ok := QoderModelFor(display)
		if !ok {
			t.Fatalf("QoderModelFor(%q) not found", display)
		}
		if info.Key != key {
			t.Errorf("QoderModelFor(%q).Key = %q, want %q", display, info.Key, key)
		}
	}
	if _, ok := QoderModelFor("gpt-6"); ok {
		t.Error("QoderModelFor(gpt-6) matched; an unknown model must fail before signing")
	}
	if _, ok := QoderModelFor(""); ok {
		t.Error("QoderModelFor() matched an empty model")
	}
}

// The turn maps onto the vendor's plaintext shape: system aside, thinking
// dropped, tool pairing intact, business always present.
func TestQoderPayloadFromRequest(t *testing.T) {
	req := StreamRequest{
		Messages: []Item{
			{Kind: "text", Role: RoleSystem, Text: "be brief"},
			{Kind: "text", Role: RoleUser, Text: "read it"},
			{Kind: "tool_call", Role: RoleAssistant, CallID: "c1", Name: "read", Args: "{}"},
			{Kind: "text", Role: RoleTool, CallID: "c1", Output: "done", Text: "done"},
			{Kind: "reasoning", Role: RoleAssistant, Text: "hmm"},
			{Kind: "text", Role: RoleUser, Text: "go on"},
		},
		Tools:     []ToolDef{{Name: "read", Description: "read a file"}},
		Reason:    ReasonHigh,
		MaxTokens: 1024,
	}
	info, _ := QoderModelFor("Qwen3.8-Flash")
	body := qoderPayloadFromRequest(req, info)
	chat := body["chat_context"].(map[string]any)
	if chat["text"] != "go on" {
		t.Errorf("chat_context.text = %v, want the last user turn", chat["text"])
	}
	if system, ok := body["system"].([]any); !ok || len(system) != 1 {
		t.Errorf("system = %v, want the system prompt aside", body["system"])
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want user/call/result/user (reasoning dropped)", len(msgs))
	}
	for _, m := range msgs {
		if _, ok := m.(map[string]any)["reasoning"]; ok {
			t.Error("a reasoning block travelled; this endpoint has no use for it")
		}
	}
	call := msgs[1].(map[string]any)
	calls, ok := call["tool_calls"].([]qodersign.ToolCall)
	if !ok || len(calls) != 1 || calls[0].ID != "c1" || calls[0].Function.Name != "read" {
		t.Errorf("assistant tool_calls = %v, want the single c1/read call", call["tool_calls"])
	}
	tool := msgs[2].(map[string]any)
	if tool["tool_call_id"] != "c1" || tool["content"] != "done" {
		t.Errorf("tool turn = %v, want c1 with its result", tool)
	}
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want the one definition", body["tools"])
	}
	params := body["parameters"].(map[string]any)
	if params["max_tokens"] != 1024 || params["reasoning_effort"] != "high" || params["enable_thinking"] != true {
		t.Errorf("parameters = %v, want max_tokens + high effort", params)
	}
	if body["business"] == nil {
		t.Error("business missing: the server routes this to a dead node")
	}
}

// Envelope shapes: string bodies, object bodies, heartbeats, refusals, and
// bare frames each take their own path.
func TestUnwrapQoderEnvelope(t *testing.T) {
	inner, skip, fail := unwrapQoderEnvelope([]byte(`{"body": "{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}"}`))
	if skip || fail != nil || !strings.Contains(inner, `"content":"hi"`) {
		t.Errorf("string body: inner=%q skip=%v fail=%v", inner, skip, fail)
	}
	inner, skip, fail = unwrapQoderEnvelope([]byte(`{"body": {"choices": [{"delta": {}}]}}`))
	if skip || fail != nil || !strings.Contains(inner, `"choices"`) {
		t.Errorf("object body: inner=%q skip=%v fail=%v", inner, skip, fail)
	}
	for _, heartbeat := range []string{``, `{"body": null}`, `{"body": ""}`, `{"body": {}}`, `not json at all`} {
		if _, skip, fail := unwrapQoderEnvelope([]byte(heartbeat)); !skip || fail != nil {
			t.Errorf("heartbeat %q: skip=%v fail=%v, want drop", heartbeat, skip, fail)
		}
	}
	for _, refusal := range []string{
		`{"model_error": "Execution failed"}`,
		`{"code": "101", "message": "Signature invalid"}`,
		`{"body": null, "error": "auth_error"}`,
	} {
		if _, skip, fail := unwrapQoderEnvelope([]byte(refusal)); skip || fail == nil {
			t.Errorf("refusal %q: skip=%v fail=%v, want failure", refusal, skip, fail)
		}
	}
}

// A full signed round trip against a stub: content streams, usage lands, the
// request carries a COSY signature at the right path.
func TestQoderTransportStreamsContent(t *testing.T) {
	var mu atomic.Int32
	var auth, path string
	var bodies int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Add(1)
		auth, path = r.Header.Get("Authorization"), r.URL.Path+"?"+r.URL.RawQuery
		atomic.AddInt32(&bodies, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"body\": \"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"hello\\\"}}]}\"}\n\n")
		fmt.Fprint(w, "data: {\"body\": {\"choices\": [{\"delta\": {}, \"finish_reason\": \"stop\"}], \"usage\": {\"input_tokens\": 10, \"output_tokens\": 5, \"total_tokens\": 15}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := &StreamingClient{
		cfg: ProviderConfig{
			Model:            "Qwen3.8-Flash",
			BaseURL:          srv.URL,
			QoderNative:      true,
			QoderCredentials: testQoderCreds(),
		},
		http: srv.Client(),
	}
	var content strings.Builder
	var usage *UsageInfo
	var done bool
	err := client.streamQoder(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(r ProviderResponse) {
		content.WriteString(r.Content)
		if r.Usage != nil {
			usage = r.Usage
		}
		if r.Done {
			done = true
		}
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if content.String() != "hello" {
		t.Errorf("content = %q, want hello", content.String())
	}
	if usage == nil || usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want the inner usage", usage)
	}
	if !done {
		t.Error("no terminal Done response")
	}
	if !strings.HasPrefix(auth, "Bearer COSY.") {
		t.Errorf("authorization = %.30q, want a COSY signature, never a plain token", auth)
	}
	if !strings.HasPrefix(path, "/algo/api/v2/service/pro/sse/agent_chat_generation") {
		t.Errorf("path = %q, want the encrypted endpoint", path)
	}
}

// A 409 rebuilds once with fresh ids and then succeeds; anything further out
// is a loop, not a recovery.
func TestQoderTransportRebuildsOnceOn409(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"duplicate_request"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"body\": \"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"ok\\\"}}]}\"}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := &StreamingClient{
		cfg: ProviderConfig{
			Model:            "Qwen3.8-Flash",
			BaseURL:          srv.URL,
			QoderNative:      true,
			QoderCredentials: testQoderCreds(),
		},
		http: srv.Client(),
	}
	var content strings.Builder
	err := client.streamQoder(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(r ProviderResponse) { content.WriteString(r.Content) })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("requests = %d, want exactly the initial plus one rebuild", got)
	}
	if content.String() != "ok" {
		t.Errorf("content = %q", content.String())
	}
}

// An auth rejection refreshes the credential exactly once before failing.
// The refresher is the only second chance; a third attempt would just spend
// a rotated token against the same refusal.
func TestQoderTransportRefreshesOnceOn401(t *testing.T) {
	creds := testQoderCreds()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"105","message":"auth_error"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"body\": \"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"ok\\\"}}]}\"}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := &StreamingClient{
		cfg: ProviderConfig{
			Model:            "Qwen3.8-Flash",
			BaseURL:          srv.URL,
			QoderNative:      true,
			QoderCredentials: creds,
		},
		http: srv.Client(),
	}
	err := client.streamQoder(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(ProviderResponse) {})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := atomic.LoadInt32(creds.refreshed); got != 1 {
		t.Errorf("refreshes = %d, want exactly one", got)
	}
}

// A queue hold waits the server's delay and retries, rather than failing as
// an auth error or burning a refresh on a waiting room.
func TestQoderTransportWaitsOnQueue(t *testing.T) {
	creds := testQoderCreds()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"10605","message":"model_queued","delay":1}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"body\": \"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"ok\\\"}}]}\"}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := &StreamingClient{
		cfg: ProviderConfig{
			Model:            "Qwen3.8-Flash",
			BaseURL:          srv.URL,
			QoderNative:      true,
			QoderCredentials: creds,
		},
		http: srv.Client(),
	}
	start := time.Now()
	err := client.streamQoder(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(ProviderResponse) {})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("waited %v, want the server's delay honoured", elapsed)
	}
	if got := atomic.LoadInt32(creds.refreshed); got != 0 {
		t.Errorf("refreshes = %d, want none: a queue is not an auth failure", got)
	}
}
