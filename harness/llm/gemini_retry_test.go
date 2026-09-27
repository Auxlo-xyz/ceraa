package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// geminiRetryProbe drives the native Gemini path and reports the round trips made
// and what the error says.
func geminiRetryProbe(t *testing.T, status, maxRetries int) (int, string) {
	t.Helper()
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream failure"}}`))
	}))
	defer srv.Close()

	c := New(ProviderConfig{
		BaseURL: srv.URL, MaxRetries: maxRetries, HTTPClient: srv.Client(), APIKey: "k",
		RetryBackoff: time.Millisecond,
	})
	err := c.streamGemini(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(ProviderResponse) {})
	return attempts, err.Error()
}

// The native Gemini path has its own retry loop and its own terminal return, and it
// carried exactly the same bare-ProviderError gap that made a retried 502 look
// unretried on the chat-completions path. It had no coverage at all, so the fix
// there was unverified.
func TestGeminiPathReportsItsAttemptCount(t *testing.T) {
	attempts, msg := geminiRetryProbe(t, 502, 3)
	if attempts != 4 {
		t.Errorf("Gemini 502: made %d round trips, want 4", attempts)
	}
	if !strings.Contains(msg, "after 4 attempt(s)") {
		t.Errorf("the Gemini error does not report the attempt count: %q", msg)
	}
}

func TestGeminiPathFailsFastOnAPermanentError(t *testing.T) {
	attempts, msg := geminiRetryProbe(t, 400, 3)
	if attempts != 1 {
		t.Errorf("Gemini 400: made %d round trips, want 1", attempts)
	}
	if !strings.Contains(msg, "after 1 attempt(s)") {
		t.Errorf("the Gemini error does not report the attempt count: %q", msg)
	}
}
