package freebuff

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(server *httptest.Server) *Client {
	client := NewClient()
	client.WebBaseURL = server.URL
	client.APIBaseURL = server.URL
	client.HTTPClient = server.Client()
	return client
}

// TestRetriesRetryableStatusesThenSucceeds pins max 4 attempts with backoff on
// the retryable status set.
func TestRetriesRetryableStatusesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "u", "email": "dev@example.com", "authToken": "t"})
	}))
	defer server.Close()
	client := newTestClient(server)
	delays := []time.Duration{}
	client.Sleep = func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}
	user, err := client.Me(context.Background(), "token")
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "dev@example.com" {
		t.Fatalf("user = %+v", user)
	}
	if calls.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", calls.Load())
	}
	if len(delays) != 2 {
		t.Fatalf("delays = %v, want 2", delays)
	}
	// Exponential growth: the second backoff must exceed the first.
	if delays[1] <= delays[0] {
		t.Fatalf("backoff did not grow: %v", delays)
	}
	if delays[0] < RetryBaseDelay || delays[1] > RetryMaxDelay+time.Duration(RetryJitter*float64(RetryMaxDelay)) {
		t.Fatalf("backoff out of bounds: %v", delays)
	}
}

// TestDoesNotRetryNonRetryableStatus pins the fail-fast path: 400 is returned
// immediately.
func TestDoesNotRetryNonRetryableStatus(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	client := newTestClient(server)
	client.Sleep = func(ctx context.Context, d time.Duration) error { return nil }
	if _, err := client.Me(context.Background(), "token"); err == nil {
		t.Fatal("expected an error for HTTP 400")
	}
	if calls.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", calls.Load())
	}
}

// TestAuthErrorsAreDistinguished pins the rule that a 401 means re-login while a
// 5xx must not clear local state.
func TestAuthErrorsAreDistinguished(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		client := newTestClient(server)
		_, err := client.Me(context.Background(), "token")
		if err == nil || !MeAuthError(err) {
			t.Fatalf("status %d: err=%v, want an auth error", status, err)
		}
		server.Close()
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	client := newTestClient(server)
	client.Sleep = func(ctx context.Context, d time.Duration) error { return nil }
	_, err := client.Me(context.Background(), "token")
	if err == nil {
		t.Fatal("expected an error for HTTP 502")
	}
	if MeAuthError(err) {
		t.Fatalf("HTTP 502 must not be treated as an auth error: %v", err)
	}
}

// TestMeSendsBearerToken pins the Authorization header.
func TestMeSendsBearerToken(t *testing.T) {
	var got string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "u", "email": "dev@example.com"})
	}))
	defer server.Close()
	client := newTestClient(server)
	if _, err := client.Me(context.Background(), "secret-token"); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer secret-token" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestLogoutAlwaysSucceedsAndSendsIdentity pins the CLI rule that a failed
// remote logout never blocks the local clear.
func TestLogoutAlwaysSucceedsAndSendsIdentity(t *testing.T) {
	var body map[string]string
	var auth string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := newTestClient(server)
	client.Sleep = func(ctx context.Context, d time.Duration) error { return nil }
	user := User{ID: "u1", FingerprintID: "fp1", FingerprintHash: "h1"}
	if err := client.Logout(context.Background(), "token", user); err != nil {
		t.Fatalf("logout must swallow remote failures, got %v", err)
	}
	if auth != "Bearer token" {
		t.Fatalf("Authorization = %q", auth)
	}
	if body["userId"] != "u1" || body["fingerprintId"] != "fp1" || body["fingerprintHash"] != "h1" {
		t.Fatalf("logout body = %v", body)
	}
}

// TestLogoutWithoutTokenIsNoop covers the idempotent already-logged-out case.
func TestLogoutWithoutTokenIsNoop(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	client := newTestClient(server)
	if err := client.Logout(context.Background(), "", User{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("logout with no token must not make a request")
	}
}

// TestTLSFailureIsNotRetried pins the CLI's TLS exclusion: a certificate
// verification failure is reported once with an interception hint, never retried.
func TestTLSFailureIsNotRetried(t *testing.T) {
	calls := 0
	trusted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "u", "email": "dev@example.com"})
	}))
	defer trusted.Close()

	// A client with an empty trust store cannot verify the server's
	// self-signed certificate, producing a TLS verification failure.
	client := NewClient()
	client.WebBaseURL = trusted.URL
	client.APIBaseURL = trusted.URL
	base := trusted.Client()
	transport, ok := base.Transport.(*http.Transport)
	if !ok {
		t.Skip("unexpected transport type")
	}
	clone := transport.Clone()
	tlsConfig := clone.TLSClientConfig.Clone()
	tlsConfig.RootCAs = x509.NewCertPool()
	clone.TLSClientConfig = tlsConfig
	client.HTTPClient = &http.Client{Transport: clone}
	client.Sleep = func(ctx context.Context, d time.Duration) error {
		t.Error("TLS failures must not be retried")
		return nil
	}

	if _, err := client.Me(context.Background(), "token"); err == nil {
		t.Fatal("expected a TLS verification error")
	} else if !strings.Contains(err.Error(), "TLS verification failed") {
		t.Fatalf("error %q should mention TLS interception", err)
	}
	if calls != 0 {
		t.Fatalf("calls = %d, want 0", calls)
	}
	if isTLSError(nil) {
		t.Fatal("isTLSError(nil) must be false")
	}
}
