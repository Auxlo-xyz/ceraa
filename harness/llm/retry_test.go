package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// retryProbe drives one status and reports how many provider round trips were made
// and what the error says about it.
func retryProbe(t *testing.T, status, maxRetries int) (int, string) {
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
		// Production backoff is 1s/3s/8s; scaled so the retry path is testable.
		RetryBackoff: time.Millisecond,
	})
	err := c.Stream(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(ProviderResponse) {})
	return attempts, err.Error()
}

// A 502 is transient and must be retried. This was misdiagnosed from a session log
// as an unretried failure, because the terminal error was returned bare and carried
// no evidence either way.
func TestTransientStatusesAreRetried(t *testing.T) {
	for _, status := range []int{502, 429, 503, 500} {
		attempts, _ := retryProbe(t, status, 3)
		if attempts != 4 {
			t.Errorf("HTTP %d: made %d attempts, want 4 (1 initial + 3 retries)", status, attempts)
		}
	}
}

// A 400 cannot be fixed by repeating the same call, so it must fail immediately.
func TestNonTransientStatusFailsOnTheFirstAttempt(t *testing.T) {
	for _, status := range []int{400, 401, 404} {
		attempts, _ := retryProbe(t, status, 3)
		if attempts != 1 {
			t.Errorf("HTTP %d: made %d attempts, want 1; a permanent error must not be retried", status, attempts)
		}
	}
}

// The attempt count has to survive into the message. Without it a failure after
// three retries is indistinguishable from one on the first attempt, which is how a
// retried 502 came to be read as unretried.
func TestTheErrorReportsHowManyAttemptsWereMade(t *testing.T) {
	_, msg := retryProbe(t, 502, 3)
	if !strings.Contains(msg, "after 4 attempt(s)") {
		t.Errorf("the error does not report the attempt count: %q", msg)
	}
	_, msg = retryProbe(t, 400, 3)
	if !strings.Contains(msg, "after 1 attempt(s)") {
		t.Errorf("a first-attempt failure should say so: %q", msg)
	}
}

// The count must reflect the retries actually configured, not a hardcoded number.
func TestAttemptCountFollowsMaxRetries(t *testing.T) {
	for _, retries := range []int{0, 1, 2, 5} {
		attempts, msg := retryProbe(t, 502, retries)
		want := retries + 1
		if attempts != want {
			t.Errorf("MaxRetries=%d: made %d attempts, want %d", retries, attempts, want)
		}
		if !strings.Contains(msg, "after "+itoaTest(want)+" attempt(s)") {
			t.Errorf("MaxRetries=%d: error says %q, want it to report %d", retries, msg, want)
		}
	}
}

// The provider's own status and body must survive the wrapping, or the message
// becomes less useful than the bare error it replaced.
func TestWrappingPreservesTheProviderDetail(t *testing.T) {
	_, msg := retryProbe(t, 502, 1)
	for _, want := range []string{"provider error 502", "upstream failure"} {
		if !strings.Contains(msg, want) {
			t.Errorf("wrapping lost %q: %q", want, msg)
		}
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}
