package telegram

import (
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
