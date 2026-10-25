package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
		"GLM-5.2":        "gm51model",
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

func TestQoderToolResultImageContent(t *testing.T) {
	shot := testImagePNG(t)
	body := qoderPayloadFromRequest(StreamRequest{Messages: []Item{
		{Kind: "text", Role: RoleTool, CallID: "c1", Output: "attached image", Images: []string{shot}},
	}}, QoderModel{Key: "qfmodel"})
	messages := body["messages"].([]any)
	message := messages[0].(map[string]any)
	content := message["content"].([]any)
	if message["role"] != string(RoleTool) || message["tool_call_id"] != "c1" {
		t.Fatalf("tool message = %v, want role/tool_call_id preserved", message)
	}
	if len(content) != 2 {
		t.Fatalf("content = %v, want text plus image parts", content)
	}
	text := content[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "attached image" {
		t.Errorf("text part = %v, want the tool-result text", text)
	}
	image := content[1].(map[string]any)
	if image["type"] != "image_url" {
		t.Fatalf("image part type = %v, want image_url", image["type"])
	}
	imageURL := image["image_url"].(map[string]any)["url"].(string)
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(imageURL, prefix) {
		t.Fatalf("image URL = %q, want %s prefix", imageURL, prefix)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(imageURL, prefix))
	if err != nil {
		t.Fatalf("image data is not base64: %v", err)
	}
	if len(decoded) == 0 {
		t.Fatal("image data is empty")
	}
}

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

// A refusal can arrive as an object, and the envelope carries its own status.
// Both shapes used to decode as a heartbeat, which reported the symptom
// ("stream ended without a terminal event") and threw the cause away.
func TestUnwrapQoderEnvelopeRefusalsOfEveryShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"object error", `{"body":null,"error":{"message":"context length exceeded","type":"invalid_request_error"}}`, "context length exceeded"},
		{"status with body", "{\"statusCode\":\"500\",\"statusCodeValue\":500,\"body\":\"{\\\"error\\\":{\\\"message\\\":\\\"upstream exploded\\\"}}\"}", "upstream exploded"},
		{"status alone", `{"statusCode":"502","statusCodeValue":502}`, "status 502"},
		{"execution failed object", `{"execution_failed":{"message":"tool runtime died"}}`, "tool runtime died"},
	} {
		inner, skip, fail := unwrapQoderEnvelope([]byte(tc.raw))
		if skip || fail == nil {
			t.Errorf("%s: skip=%v fail=%v, want a refusal", tc.name, skip, fail)
			continue
		}
		if !strings.Contains(fail.Error(), tc.want) {
			t.Errorf("%s: fail=%q, want %q", tc.name, fail.Error(), tc.want)
		}
		if inner != "" {
			t.Errorf("%s: inner=%q, want nothing forwarded", tc.name, inner)
		}
	}
	// The live success envelope must keep streaming, not read as a refusal.
	if inner, skip, fail := unwrapQoderEnvelope([]byte(`{"headers":{"Content-Type":["application/json"]},"body":"{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}","statusCodeValue":200,"statusCode":"OK"}`)); fail != nil || skip || !strings.Contains(inner, `"content":"hi"`) {
		t.Errorf("success envelope: inner=%q skip=%v fail=%v", inner, skip, fail)
	}
}

// A cut during thinking carries no text the caller has seen, so the turn can be
// replayed. This is the failure the live DeepSeek stream produced: reasoning was
// already on the wire, the shared parser called it partial, and the retry loop
// refused to run, failing the whole run with a terminal-event error.
func TestQoderTransportRetriesACutDuringThinking(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			fmt.Fprint(w, "data: {\"body\": \"{\\\"choices\\\":[{\\\"delta\\\":{\\\"reasoning_content\\\":\\\"thinking\\\"}}]}\"}\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"body\": \"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"ok\\\"}}]}\"}\n\n")
		fmt.Fprint(w, "data: {\"body\": \"{\\\"choices\\\":[{\\\"delta\\\":{},\\\"finish_reason\\\":\\\"stop\\\"}]}\"}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := New(ProviderConfig{
		Model:            "DeepSeek-Flash",
		BaseURL:          srv.URL,
		QoderNative:      true,
		QoderCredentials: testQoderCreds(),
		MaxRetries:       1,
		RetryBackoff:     time.Millisecond,
		HTTPClient:       srv.Client(),
	})
	var content strings.Builder
	err := client.Stream(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(r ProviderResponse) { content.WriteString(r.Content) })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("requests = %d, want the initial plus one retry", got)
	}
	if content.String() != "ok" {
		t.Errorf("content = %q, want ok", content.String())
	}
}

// Text already handed to the caller is different: replaying it would duplicate
// the answer, so a cut after visible content still fails rather than retries.
func TestQoderTransportDoesNotRetryACutAfterVisibleText(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			fmt.Fprint(w, "data: {\"body\": \"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"partial\\\"}}]}\"}\n\n")
			return
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := New(ProviderConfig{
		Model:            "DeepSeek-Flash",
		BaseURL:          srv.URL,
		QoderNative:      true,
		QoderCredentials: testQoderCreds(),
		MaxRetries:       2,
		RetryBackoff:     time.Millisecond,
		HTTPClient:       srv.Client(),
	})
	var content strings.Builder
	err := client.Stream(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(r ProviderResponse) { content.WriteString(r.Content) })
	if err == nil {
		t.Fatal("stream succeeded, want the terminal-event failure")
	}
	if !strings.Contains(err.Error(), "terminal event") {
		t.Errorf("err = %v, want the terminal-event failure", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("requests = %d, want no retry after visible text", got)
	}
	if content.String() != "partial" {
		t.Errorf("content = %q, want the partial text preserved", content.String())
	}
}

// A message that carries reasoning is not a reasoning item. It used to be
// dropped whole, which removed an assistant tool call and left its result
// orphaned in the history; the host answers that with "Error in upstream
// response" and fails every tool-using turn after the first.
func TestQoderPayloadKeepsMessagesThatCarryReasoning(t *testing.T) {
	info, ok := QoderModelFor("DeepSeek-Flash")
	if !ok {
		t.Fatal("no DeepSeek-Flash model entry")
	}
	payload := qoderPayloadFromRequest(StreamRequest{Messages: []Item{
		{Kind: "text", Role: RoleSystem, Text: "you are a test"},
		{Kind: "text", Role: RoleUser, Text: "run echo hi"},
		{Kind: "tool_call", Role: RoleAssistant, CallID: "call-1", Name: "bash", Args: `{"command":"echo hi"}`, Reason: "Run the command."},
		{Kind: "text", Role: RoleTool, CallID: "call-1", Name: "bash", Output: "hi"},
		{Kind: "reasoning", Role: RoleAssistant, Reason: "standalone thought"},
		{Kind: "text", Role: RoleAssistant, Text: "done", Reason: "Explain the result."},
		{Kind: "text", Role: RoleUser, Text: "thanks"},
	}}, info)

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var body struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode payload: %v\n%s", err, raw)
	}
	history := body.Messages
	// The two users are the opening turn and the closing one; the standalone
	// reasoning item is the only thing dropped.
	if len(history) != 5 {
		t.Fatalf("history has %d entries, want 5: user, assistant call, tool result, assistant text, user\n%s", len(history), raw)
	}
	if history[0].Role != "user" || history[4].Role != "user" {
		t.Errorf("user turns missing: %+v", history)
	}
	if len(history[1].ToolCalls) != 1 || history[1].ToolCalls[0].ID != "call-1" {
		t.Errorf("assistant tool call missing from history: %+v", history[1])
	}
	if history[2].Role != "tool" || history[2].ToolCallID != "call-1" {
		t.Errorf("tool result lost its call: %+v", history[2])
	}
	if history[3].Role != "assistant" || !strings.Contains(string(history[3].Content), "done") {
		t.Errorf("assistant text missing from history: %+v", history[3])
	}
}

// The host reports a capacity hold as a 200 stream carrying a 403 envelope
// whose message is itself a nested 10605 frame. It has to classify as a wait,
// not a refusal: the frame names the delay because the model comes back.
func TestUnwrapQoderEnvelopeReadsACapacityHold(t *testing.T) {
	raw := `{"body":"{\"code\":\"403\",\"message\":\"{\\\"code\\\":\\\"10605\\\",\\\"message\\\":\\\"{\\\\\\\"isQueued\\\\\\\":true,\\\\\\\"modelKey\\\\\\\":\\\\\\\"dfmodel\\\\\\\",\\\\\\\"queueCount\\\\\\\":0,\\\\\\\"queueType\\\\\\\":\\\\\\\"p3\\\\\\\",\\\\\\\"retryAfterSeconds\\\\\\\":30,\\\\\\\"serviceAvailable\\\\\\\":false,\\\\\\\"subCode\\\\\\\":\\\\\\\"RESOURCE_LIMIT\\\\\\\",\\\\\\\"waitTime\\\\\\\":30}\\\",\\\"subCode\\\":\\\"RESOURCE_LIMIT\\\"}\"}","statusCodeValue":403,"statusCode":"FORBIDDEN"}`
	_, _, fail := unwrapQoderEnvelope([]byte(raw))
	if fail == nil {
		t.Fatal("hold decoded as a heartbeat")
	}
	refusal, ok := fail.(*qoderModelError)
	if !ok {
		t.Fatalf("fail type = %T", fail)
	}
	if refusal.queue != 30*time.Second {
		t.Errorf("queue = %v, want 30s", refusal.queue)
	}
	if refusal.pricing {
		t.Error("a hold is not a plan refusal")
	}
	if !strings.Contains(refusal.Error(), "capacity") {
		t.Errorf("error = %q, want it to name the hold", refusal.Error())
	}
}

// A model outside the plan is permanent for this account: no wait, and the
// message should point at the plan rather than echo a bare pricingUrl object.
func TestUnwrapQoderEnvelopeReadsAPlanRefusal(t *testing.T) {
	_, _, fail := unwrapQoderEnvelope([]byte(`{"body":"{\"code\":\"112\",\"message\":\"{\\\"pricingUrl\\\":\\\"https://qoder.com/pricing?client=qoder\\\"}\"}","statusCodeValue":403,"statusCode":"FORBIDDEN"}`))
	refusal, ok := fail.(*qoderModelError)
	if !ok {
		t.Fatalf("fail = %v (%T), want a refusal", fail, fail)
	}
	if refusal.queue != 0 {
		t.Errorf("queue = %v, a plan refusal must not wait", refusal.queue)
	}
	if !refusal.pricing {
		t.Error("the pricing payload was not recognised")
	}
	if !strings.Contains(refusal.Error(), "not included") {
		t.Errorf("error = %q, want the plan wording", refusal.Error())
	}
}

// A hold on the stream waits its named delay and then succeeds, without
// spending the network-retry budget on it.
func TestQoderTransportWaitsOutACapacityHold(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			fmt.Fprint(w, "data: {\"body\": \"{\\\"code\\\":\\\"10605\\\",\\\"message\\\":\\\"{\\\\\\\"isQueued\\\\\\\":true,\\\\\\\"retryAfterSeconds\\\\\\\":1,\\\\\\\"subCode\\\\\\\":\\\\\\\"RESOURCE_LIMIT\\\\\\\"}\\\"}\",\"statusCodeValue\":403,\"statusCode\":\"FORBIDDEN\"}\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"body\": \"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"ok\\\"}}]}\"}\n\n")
		fmt.Fprint(w, "data: {\"body\": \"{\\\"choices\\\":[{\\\"delta\\\":{},\\\"finish_reason\\\":\\\"stop\\\"}]}\"}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := New(ProviderConfig{
		Model:            "DeepSeek-Flash",
		BaseURL:          srv.URL,
		QoderNative:      true,
		QoderCredentials: testQoderCreds(),
		MaxRetries:       0,
		RetryBackoff:     time.Millisecond,
		HTTPClient:       srv.Client(),
	})
	var content strings.Builder
	start := time.Now()
	err := client.Stream(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(r ProviderResponse) { content.WriteString(r.Content) })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("requests = %d, want the hold then the answer", got)
	}
	if content.String() != "ok" {
		t.Errorf("content = %q, want ok", content.String())
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("waited %v, want the frame's delay honoured", elapsed)
	}
}

// The picker (QoderModels) and the transport directory (QoderModelTable) are
// separate lists, so a name added to one and not the other is selectable and
// then refused before signing, which reads as "unknown model" for a model the
// menu just offered.
func TestEveryQoderPickerNameResolvesToAKey(t *testing.T) {
	for _, name := range QoderModels {
		if _, ok := QoderModelFor(name); !ok {
			t.Errorf("picker offers %q but the transport directory has no entry for it", name)
		}
	}
}
