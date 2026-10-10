package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Reasoning levels land on upstream tiers, and unknown levels take the
// reference default rather than failing the turn.
func TestCodeAssistTierMapping(t *testing.T) {
	for level, tier := range map[ReasonLevel]string{
		"":            "medium",
		ReasonOff:     "low",
		ReasonMinimal: "low",
		ReasonLow:     "low",
		ReasonMedium:  "medium",
		ReasonHigh:    "high",
		ReasonXHigh:   "high",
		ReasonMax:     "high",
	} {
		if got := CodeAssistTier(level); got != tier {
			t.Errorf("CodeAssistTier(%q) = %q, want %q", level, got, tier)
		}
	}
}

// The envelope is the v1internal shape, not the public one: routing fields
// around a standard request, with the tier in the model id and a stable
// conversation id for upstream cache hits.
func TestBuildCodeAssistBodyEnvelope(t *testing.T) {
	req := StreamRequest{
		Messages:  []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
		MaxTokens: 1024,
		SessionID: "sess-9",
	}
	body, err := buildCodeAssistBody(req, "medium")
	if err != nil {
		t.Fatal(err)
	}
	if body["model"] != "gemini-3.8-flash-medium" {
		t.Errorf("model = %v, want the tier-suffixed upstream id", body["model"])
	}
	if body["project"] != CodeAssistProject {
		t.Errorf("project = %v", body["project"])
	}
	if body["userAgent"] != "antigravity" {
		t.Errorf("userAgent = %v", body["userAgent"])
	}
	request := body["request"].(map[string]any)
	if request["sessionId"] != "ceraa-sess-9" {
		t.Errorf("sessionId = %v, want the stable Ceraa session", request["sessionId"])
	}
	gen, ok := request["generationConfig"].(*geminiGenerationConfig)
	if !ok || gen.MaxOutputTokens != 1024 {
		t.Errorf("generationConfig = %#v, want maxOutputTokens 1024", request["generationConfig"])
	}
	if gen.ThinkingConfig == nil || !gen.ThinkingConfig.IncludeThoughts || gen.ThinkingConfig.ThinkingBudget == nil || *gen.ThinkingConfig.ThinkingBudget != 4000 {
		t.Errorf("thinkingConfig = %#v, want includeThoughts with the medium budget", gen.ThinkingConfig)
	}
	if _, err := json.Marshal(body); err != nil {
		t.Errorf("envelope does not encode: %v", err)
	}
}

type stubCodeAssistCreds struct {
	token     string
	refreshed *int32
}

func (s stubCodeAssistCreds) AccessToken(context.Context) (string, error) {
	return s.token, nil
}

func (s stubCodeAssistCreds) RefreshToken(context.Context) (string, error) {
	atomic.AddInt32(s.refreshed, 1)
	return s.token, nil
}

func geminiSSE() string {
	return "data: {\"candidates\": [{\"content\": {\"role\": \"model\", \"parts\": [{\"text\": \"hello\"}]}}]}\n\n" +
		"data: {\"candidates\": [{\"finishReason\": \"STOP\"}], \"usageMetadata\": {\"promptTokenCount\": 10, \"candidatesTokenCount\": 5, \"totalTokenCount\": 15}}\n\n"
}

// A full signed round trip against a stub: the URL is the v1internal method,
// auth is a bearer plus the client identity, and the envelope carries the
// tiered model, the project and a session.
func TestCodeAssistTransportStreamsContent(t *testing.T) {
	var auth, ua, path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, ua, path = r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(geminiSSE()))
	}))
	defer srv.Close()

	var refreshed int32
	client := &StreamingClient{
		cfg: ProviderConfig{
			Model:            "Gemini 3.8 Flash",
			BaseURL:          srv.URL,
			CodeAssistNative: true,
			CodeAssistTokens: stubCodeAssistCreds{token: "tok-test", refreshed: &refreshed},
		},
		http: srv.Client(),
	}
	var content strings.Builder
	var done bool
	err := client.streamCodeAssist(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(r ProviderResponse) {
		content.WriteString(r.Content)
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
	if !done {
		t.Error("no terminal Done response")
	}
	if auth != "Bearer tok-test" {
		t.Errorf("authorization = %q, want the OAuth bearer", auth)
	}
	if ua != "antigravity/4.3.0 (cmdc-pak)" {
		t.Errorf("user-agent = %q, want the client identity", ua)
	}
	if path != "/v1internal:streamGenerateContent" {
		t.Errorf("path = %q, want the streaming method", path)
	}
	if body["model"] != "gemini-3.8-flash-medium" {
		t.Errorf("envelope model = %v", body["model"])
	}
	if body["project"] != CodeAssistProject {
		t.Errorf("envelope project = %v", body["project"])
	}
}

// A 401 renews the grant once and retries; anything further out is a loop.
func TestCodeAssistTransportRefreshesOnceOn401(t *testing.T) {
	var calls int32
	var refreshed int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error": "auth_error"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(geminiSSE()))
	}))
	defer srv.Close()

	client := &StreamingClient{
		cfg: ProviderConfig{
			Model:            "Gemini 3.8 Flash",
			BaseURL:          srv.URL,
			CodeAssistNative: true,
			CodeAssistTokens: stubCodeAssistCreds{token: "tok-test", refreshed: &refreshed},
		},
		http: srv.Client(),
	}
	err := client.streamCodeAssist(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(ProviderResponse) {})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := atomic.LoadInt32(&refreshed); got != 1 {
		t.Errorf("refreshes = %d, want exactly one", got)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("requests = %d, want the initial plus one retry", got)
	}
}

// Failover retries only across endpoints, never twice on one: 403/404 and
// quota-shaped 400s move, everything else fails where it stands.
func TestCodeAssistFailoverPredicate(t *testing.T) {
	for status, body := range map[int]string{
		http.StatusForbidden:    `{}`,
		http.StatusNotFound:     `{}`,
		http.StatusBadRequest:   `{"error": "quota exhausted"}`,
		http.StatusUnauthorized: `{"error": "auth_error"}`,
	} {
		want := status != http.StatusUnauthorized
		if got := codeAssistFailover(status, []byte(body)); got != want {
			t.Errorf("failover(%d, %s) = %v, want %v", status, body, got, want)
		}
	}
	if codeAssistFailover(http.StatusBadRequest, []byte(`{"error": "bad request"}`)) {
		t.Error("a plain 400 fails over; only quota-shaped ones should")
	}
	if codeAssistFailover(http.StatusInternalServerError, []byte(`{}`)) {
		t.Error("a 500 fails over; endpoint rotation is not a quota strategy")
	}
}
