package telegram

import (
	"ceraa/harness/llm"
	"ceraa/internal/opencodeauth"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestVerifyProviderSucceeds pins that a working provider passes.
func TestVerifyProviderSucceeds(t *testing.T) {
	var gotAuth, gotPath string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer server.Close()
	if err := verifyProvider(context.Background(), server.URL, "key-1", "m1"); err != nil {
		t.Fatalf("verifyProvider: %v", err)
	}
	if gotAuth != "Bearer key-1" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	if body["stream"] != false {
		t.Fatal("verification must not stream")
	}
}

// TestProviderCompletionsURL pins URL normalization: a trailing slash and a
// base that already names the path must both produce one clean endpoint.
func TestProviderCompletionsURL(t *testing.T) {
	cases := map[string]string{
		"https://x.example/v1":                                    "https://x.example/v1/chat/completions",
		"https://x.example/v1/":                                   "https://x.example/v1/chat/completions",
		"https://x.example/v1///":                                 "https://x.example/v1/chat/completions",
		"https://x.example/v1/chat/completions":                   "https://x.example/v1/chat/completions",
		"https://x.example/v1/chat/completions/":                  "https://x.example/v1/chat/completions",
		"https://generativelanguage.googleapis.com/v1beta/openai": "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
	}
	for base, want := range cases {
		got, err := providerCompletionsURL(base)
		if err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		if got != want {
			t.Errorf("%s = %q, want %q", base, got, want)
		}
	}
	for _, bad := range []string{"", "   ", "x.example/v1", "ftp://x/v1"} {
		if _, err := providerCompletionsURL(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// TestProviderErrorText pins the message extraction across the shapes providers
// actually return. A bare "provider error 400: <json blob>" is useless to a
// user deciding which of the three values they got wrong.
func TestProviderErrorText(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"openai string error", `{"error":{"message":"Invalid API key","type":"auth"}}`, "Invalid API key"},
		{"gemini array error", `[{"error":{"code":400,"message":"Please pass a valid API key","status":"INVALID_ARGUMENT"}}]`, "Please pass a valid API key"},
		{"top level message", `{"message":"model not found"}`, "model not found"},
		{"detail field", `{"detail":"unsupported model"}`, "unsupported model"},
		{"plain text", `upstream connect error`, "upstream connect error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerErrorText([]byte(tc.body)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	if got := providerErrorText(nil); !strings.Contains(got, "empty") {
		t.Fatalf("empty body = %q", got)
	}
}

// TestVerifyProviderReportsProviderError pins that a non-2xx becomes a message
// naming the status and the provider's own explanation.
func TestVerifyProviderReportsProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`[{"error":{"code":400,"message":"Please pass a valid API key"}}]`))
	}))
	defer server.Close()
	err := verifyProvider(context.Background(), server.URL, "bad", "m1")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "valid API key") {
		t.Fatalf("error = %q, want the status and the provider message", err)
	}
}

// TestVerifyProviderRejectsEmptyModel keeps a missing model from being reported
// as a provider fault.
func TestVerifyProviderRejectsEmptyModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be sent without a model")
	}))
	defer server.Close()
	if err := verifyProvider(context.Background(), server.URL, "k", "  "); err == nil {
		t.Fatal("expected an error for an empty model")
	}
}

// TestVerifyProviderFailsLoudlyOnUnreachableHost pins that an unreachable host
// is a verification failure rather than a silent pass.
func TestVerifyProviderFailsLoudlyOnUnreachableHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := server.URL
	server.Close()
	if err := verifyProvider(context.Background(), endpoint, "k", "m"); err == nil {
		t.Fatal("expected an error for a closed host")
	}
}

// Zen's gateway reads the header contract and the body shape as one thing, so
// the verification request has to carry both or it fails for a reason that has
// nothing to do with the key that was just typed. Measured 2026-10-01: a
// correct header set on a non-streaming ping with no tools comes back
// FreeTierError, and the caller is then told to re-check a good key.
func TestVerifyProviderShapesTheBodyForTheOpenCodeContract(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte("data: {\"choices\":[]}\n\ndata: [DONE]\n"))
	}))
	defer server.Close()

	headers := opencodeauth.RequestHeaders("verify", "")
	if err := verifyProviderWithHeaders(context.Background(), server.URL, "zk-1", "m1", headers); err != nil {
		t.Fatalf("verifyProviderWithHeaders: %v", err)
	}
	if body["stream"] != true {
		t.Errorf("stream = %v, want true; the free tier refuses a request that does not stream", body["stream"])
	}
	got := map[string]bool{}
	for _, entry := range body["tools"].([]any) {
		tool := entry.(map[string]any)
		function := tool["function"].(map[string]any)
		got[function["name"].(string)] = true
	}
	for _, want := range llm.OpenCodeTrafficTools {
		if !got[want] {
			t.Errorf("tools = %v, missing %q, which the free tier looks for", got, want)
		}
	}
	// The verification sends no tools of its own. The pair added to satisfy
	// the gateway used to be marked uncallable with "none", but the gateway
	// now refuses "none", so the key must be absent. A verification ping
	// never executes an answer.
	if _, present := body["tool_choice"]; present {
		t.Errorf("tool_choice is present on a verification ping; the gateway refuses \"none\" and absent passes")
	}
	// The injected pair must carry the gateway markers with coherent
	// schemas, or the verification fails for a reason that reads like a bad
	// key. This is the seam the contract lives or dies on.
	for _, entry := range body["tools"].([]any) {
		tool := entry.(map[string]any)
		function := tool["function"].(map[string]any)
		name := function["name"].(string)
		marker := llm.OpenCodeMarkerFor(name)
		if marker == "" {
			continue
		}
		if desc, _ := function["description"].(string); !strings.Contains(desc, marker) {
			t.Errorf("injected tool %q lacks the gateway marker", name)
		}
	}
}

// Every other provider still gets the plain non-streaming ping it had. A check
// that streamed for everyone would turn a working provider into a failure
// wherever the endpoint has no streaming to offer.
func TestVerifyProviderLeavesOtherProvidersAlone(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer server.Close()
	if err := verifyProvider(context.Background(), server.URL, "k", "m1"); err != nil {
		t.Fatalf("verifyProvider: %v", err)
	}
	if body["stream"] != false {
		t.Errorf("stream = %v, want false for a provider with no header contract", body["stream"])
	}
	if _, ok := body["tools"]; ok {
		t.Error("tools were added to a provider that asked for none")
	}
}
