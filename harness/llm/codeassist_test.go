package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The v1internal method wraps chunks as {"response": {...}}; the public
// method sends them bare. Without the unwrap every frame parses empty: no
// content, no finish reason, and the stream dies with "ended without
// finish_reason" on a perfectly good reply. Usage-only frames keep reading
// rather than terminating.
func TestCodeAssistStreamUnwrapsResponseEnvelopes(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"response": {"candidates": [{"content": {"role": "model", "parts": [{"text": "hello"}]}}]}}`,
		`data: {"response": {"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15}}}`,
		`data: {"response": {"candidates": [{"finishReason": "STOP"}]}}`,
		`data: [DONE]`,
		"",
	}, "\n")

	var content, finish string
	var usage *UsageInfo
	client := &StreamingClient{}
	err := client.readGeminiStream(strings.NewReader(stream), func(delta ProviderResponse) {
		content += delta.Content
		if delta.FinishReason != "" {
			finish = delta.FinishReason
		}
		if delta.Usage != nil {
			usage = delta.Usage
		}
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if content != "hello" {
		t.Errorf("content = %q, want hello", content)
	}
	if finish != "stop" {
		t.Errorf("finish = %q, want stop", finish)
	}
	if usage == nil || usage.TotalTokens != 15 {
		t.Errorf("usage = %+v, want the usage-only frame counted", usage)
	}
}

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
	request := body["request"].(codeAssistRequest)
	if request.SessionID != "ceraa-sess-9" {
		t.Errorf("sessionId = %v, want the stable Ceraa session", request.SessionID)
	}
	if request.GenerationConfig == nil || request.GenerationConfig.MaxOutputTokens != 1024 {
		t.Errorf("generationConfig = %#v, want maxOutputTokens 1024", request.GenerationConfig)
	}
	gen := request.GenerationConfig
	if gen.ThinkingConfig == nil || !gen.ThinkingConfig.IncludeThoughts || gen.ThinkingConfig.ThinkingBudget == nil || *gen.ThinkingConfig.ThinkingBudget != 4000 {
		t.Errorf("thinkingConfig = %#v, want includeThoughts with the medium budget", gen.ThinkingConfig)
	}
	if _, err := json.Marshal(body); err != nil {
		t.Errorf("envelope does not encode: %v", err)
	}
}

// Pins the field order: the regions that are stable within a conversation are
// declared before the one that is not, so contents is last.
//
// What this does and does not claim is worth being precise about, because the
// ordering is easy to "explain" with a mechanism that is probably wrong. The
// provider cannot be matching raw request bytes: this transport mints a fresh
// random requestId per request and still cached at a 91.8% steady state, so it is
// matching parsed structure. This test therefore pins the ORDERING PROPERTY, not
// a proven cause of any cache miss - it stops the field order drifting back
// silently, and it is the one thing about the ordering we can actually assert.
func TestCodeAssistSerialisesTheStablePrefixBeforeTheConversation(t *testing.T) {
	req := StreamRequest{
		Messages: []Item{
			{Kind: "text", Role: RoleSystem, Text: "SYSTEM-PROMPT-MARKER"},
			{Kind: "text", Role: RoleUser, Text: "hello"},
		},
		Tools:     []ToolDef{{Name: "TOOL-ARRAY-MARKER", Description: "d"}},
		MaxTokens: 1024,
		SessionID: "sess-9",
	}
	body, err := buildCodeAssistBody(req, "medium")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(body["request"])
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	system := strings.Index(text, "SYSTEM-PROMPT-MARKER")
	tools := strings.Index(text, "TOOL-ARRAY-MARKER")
	contents := strings.Index(text, `"contents"`)
	switch {
	case system < 0 || tools < 0:
		t.Fatalf("markers missing from the encoded request: %s", text)
	case contents < 0:
		t.Fatal("contents missing from the encoded request")
	case system > contents:
		t.Errorf("systemInstruction serialises after contents, so the cache is capped at byte zero:\n%s", text)
	case tools > contents:
		t.Errorf("tools serialise after contents, so the largest stable block is never cacheable:\n%s", text)
	}
}

// The same ordering property for the native Gemini transport, which is a
// separate struct and therefore a separate chance to get the order wrong.
func TestGeminiSerialisesTheStablePrefixBeforeTheConversation(t *testing.T) {
	req := StreamRequest{
		Messages: []Item{
			{Kind: "text", Role: RoleSystem, Text: "SYSTEM-PROMPT-MARKER"},
			{Kind: "text", Role: RoleUser, Text: "hello"},
		},
		Tools:     []ToolDef{{Name: "TOOL-ARRAY-MARKER", Description: "d"}},
		MaxTokens: 1024,
	}
	encoded, err := json.Marshal(buildGeminiRequest(req, "gemini-3.5-flash"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	system := strings.Index(text, "SYSTEM-PROMPT-MARKER")
	tools := strings.Index(text, "TOOL-ARRAY-MARKER")
	contents := strings.Index(text, `"contents"`)
	switch {
	case system < 0 || tools < 0:
		t.Fatalf("markers missing from the encoded request: %s", text)
	case contents < 0:
		t.Fatal("contents missing from the encoded request")
	case system > contents:
		t.Errorf("systemInstruction serialises after contents:\n%s", text)
	case tools > contents:
		t.Errorf("tools serialise after contents:\n%s", text)
	}
}

// Two requests differing only by an appended turn must produce byte-identical
// bytes for everything ahead of the conversation, which is what makes a prefix
// reusable as the session grows.
//
// The cut point is the index of "contents", and the assertion is on the region
// before it. An earlier version of this test stripped requestId defensively;
// that was a no-op, because requestId is a sibling of request in the envelope and
// therefore can never fall inside a region that ends at contents. This version
// asserts the markers are actually present, so a body that lost its stable head
// entirely cannot pass by matching two empty strings.
func TestGrowingTheConversationLeavesTheStablePrefixByteIdentical(t *testing.T) {
	base := StreamRequest{
		Messages: []Item{
			{Kind: "text", Role: RoleSystem, Text: "SYSTEM-PROMPT-MARKER"},
			{Kind: "text", Role: RoleUser, Text: "first"},
		},
		Tools:     []ToolDef{{Name: "TOOL-ARRAY-MARKER", Description: "d"}},
		MaxTokens: 1024,
		SessionID: "sess-9",
	}
	grown := base
	grown.Messages = append(append([]Item(nil), base.Messages...), Item{Kind: "text", Role: RoleUser, Text: "second"})

	head := func(body map[string]any) string {
		encoded, err := json.Marshal(body["request"])
		if err != nil {
			t.Fatalf("encoding the request: %v", err)
		}
		s := string(encoded)
		cut := strings.Index(s, `"contents"`)
		if cut < 0 {
			t.Fatalf("contents missing from the encoded request: %s", s)
		}
		if !strings.Contains(s, "SYSTEM-PROMPT-MARKER") || !strings.Contains(s, "TOOL-ARRAY-MARKER") {
			t.Fatalf("the stable head is missing from the body, so there is nothing to compare: %s", s)
		}
		return s[:cut]
	}
	first, err := buildCodeAssistBody(base, "medium")
	if err != nil {
		t.Fatalf("building the first body: %v", err)
	}
	second, err := buildCodeAssistBody(grown, "medium")
	if err != nil {
		t.Fatalf("building the second body: %v", err)
	}
	if a, b := head(first), head(second); a != b {
		t.Errorf("the stable prefix changed when the conversation grew:\nfirst:  %s\nsecond: %s", a, b)
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

// The explicit subscription flag beats native auto-detection: this host and
// model match the native heuristics, so detection alone routes at the public
// endpoint and the server 404s on /models/.... This is the exact misroute
// that shipped.
func TestCodeAssistNativeWinsOverAutoDetection(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(geminiSSE()))
	}))
	defer srv.Close()

	cfg := FromProvider("https://daily-cloudcode-pa.googleapis.com", "", "Gemini 3.8 Flash")
	if !cfg.GeminiNative {
		t.Fatal("test setup wrong: this host+model must trip auto-detection")
	}
	// Redirect at the stub: the flags mirror production, the traffic stays local.
	cfg.BaseURL = srv.URL
	cfg.CodeAssistNative = true
	cfg.CodeAssistTokens = stubCodeAssistCreds{token: "tok-test", refreshed: new(int32)}
	client := New(cfg)
	err := client.Stream(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(ProviderResponse) {})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !strings.HasPrefix(path, "/v1internal:") {
		t.Errorf("path = %q, want the v1internal method, not the public models path", path)
	}
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

// The account-verification 403 carries Google's second-step link in
// details[].metadata. Parsing it is what turns a dead-end JSON body into an
// actionable message.
func TestCodeAssistValidationErrorFrom403(t *testing.T) {
	const payload = `{
  "error": {
    "code": 403,
    "message": "Verify your account to continue.",
    "status": "PERMISSION_DENIED",
    "details": [
      {
        "@type": "type.googleapis.com/google.rpc.ErrorInfo",
        "reason": "VALIDATION_REQUIRED",
        "domain": "cloudcode-pa.googleapis.com",
        "metadata": {
          "validation_learn_more_url": "https://support.google.com/accounts?p=al_alert",
          "validation_error_message": "Verify your account to continue.",
          "validation_url_link_text": "Verify your account",
          "validation_url": "https://accounts.google.com/signin/continue?sarp=1&scc=1&continue=https://developers.google.com/gemini-code-assist/auth/auth_success_gemini"
        }
      },
      {
        "@type": "type.googleapis.com/google.rpc.Help",
        "links": [
          {"description": "Verify your account", "url": "https://accounts.google.com/signin/continue?sarp=1"},
          {"description": "Learn more", "url": "https://support.google.com/accounts?p=al_alert"}
        ]
      }
    ]
  }
}`
	got := codeAssistValidationFromBody([]byte(payload))
	if got == nil {
		t.Fatal("no validation error parsed from the 403 payload")
	}
	if !strings.Contains(got.URL, "accounts.google.com/signin/continue") {
		t.Errorf("url = %q, want the signin/continue link", got.URL)
	}
	if got.LinkText != "Verify your account" {
		t.Errorf("link text = %q", got.LinkText)
	}
	if !strings.Contains(got.LearnMoreURL, "support.google.com") {
		t.Errorf("learn-more url = %q", got.LearnMoreURL)
	}
	text := got.Error()
	if !strings.Contains(text, got.URL) || !strings.Contains(text, "verification") {
		t.Errorf("error text is not actionable:\n%s", text)
	}
	if strings.Contains(text, `"details"`) {
		t.Errorf("error text still carries the raw provider body:\n%s", text)
	}
}

// A non-TLS link never becomes a clickable instruction, even if the provider
// body carries one.
func TestCodeAssistValidationErrorRejectsPlainHTTP(t *testing.T) {
	payload := []byte(`{"error":{"message":"Verify your account to continue.","details":[{"metadata":{"validation_url":"http://accounts.google.com/signin/continue"}}]}}`)
	if got := codeAssistValidationFromBody(payload); got != nil {
		t.Fatalf("parsed a plain-http validation link: %+v", got)
	}
}

// The transport must stop at the account-verification 403: the sandbox host
// returns the same account-level refusal, so a failover only doubles the wait
// and still ends in the same dead end.
func TestCodeAssistStreamSurfacesValidationLink(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Verify your account to continue.","details":[{"metadata":{"validation_url":"https://accounts.google.com/signin/continue?sarp=1","validation_url_link_text":"Verify your account","validation_error_message":"Verify your account to continue."}}]}}`))
	}))
	defer srv.Close()

	client := &StreamingClient{
		cfg: ProviderConfig{
			Model:            "Gemini 3.8 Flash",
			BaseURL:          srv.URL,
			CodeAssistNative: true,
			CodeAssistTokens: stubCodeAssistCreds{token: "tok-test", refreshed: new(int32)},
		},
		http: srv.Client(),
	}
	err := client.streamCodeAssist(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(ProviderResponse) {})
	var validation *CodeAssistValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error = %v, want a CodeAssistValidationError", err)
	}
	if !strings.Contains(validation.URL, "accounts.google.com/signin/continue") {
		t.Errorf("url = %q", validation.URL)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("requests = %d, want one (no sandbox failover for account verification)", got)
	}
}

// The post-login probe reads the same requirement from loadCodeAssist, so the
// flow can present step 2 before a task is attempted.
func TestProbeCodeAssistAccountReportsValidation(t *testing.T) {
	var path, auth string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"ineligibleTiers":[{"reasonCode":"VALIDATION_REQUIRED","reasonMessage":"Verify your account to continue.","validationUrl":"https://accounts.google.com/signin/continue?sarp=1","validationUrlLinkText":"Verify your account","validationErrorMessage":"Verify your account to continue.","validationLearnMoreUrl":"https://support.google.com/accounts?p=al_alert"}]}`))
	}))
	defer srv.Close()

	got, err := probeCodeAssistAccount(context.Background(), srv.URL, stubCodeAssistCreds{token: "tok-test", refreshed: new(int32)})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got == nil || !strings.Contains(got.URL, "accounts.google.com/signin/continue") {
		t.Fatalf("probe = %+v, want the verification link", got)
	}
	if path != "/v1internal:loadCodeAssist" {
		t.Errorf("path = %q, want the loadCodeAssist method", path)
	}
	if auth != "Bearer tok-test" {
		t.Errorf("authorization = %q", auth)
	}
	metadata, _ := body["metadata"].(map[string]any)
	if metadata["pluginType"] != "GEMINI" {
		t.Errorf("metadata = %v, want the reference client metadata", metadata)
	}
}

// A usable account (currentTier present) must not produce a phantom step 2.
func TestProbeCodeAssistAccountAcceptsCurrentTier(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"currentTier":{"id":"free-tier"},"cloudaicompanionProject":"aicode-consumers"}`))
	}))
	defer srv.Close()
	got, err := probeCodeAssistAccount(context.Background(), srv.URL, stubCodeAssistCreds{token: "tok-test", refreshed: new(int32)})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got != nil {
		t.Fatalf("probe = %+v, want nil for a usable account", got)
	}
}
