package freebuff

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"ceraa/internal/secret"
)

func initTestSecret(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	if err := secret.Init(filepath.Join(root, "key"), filepath.Join(root, "secrets.json")); err != nil {
		t.Fatal(err)
	}
}

// TestDeviceLoginStoresCredential covers the happy path: request a code, poll
// through a 401 "not approved yet" response, then persist the approved user.
func TestDeviceLoginStoresCredential(t *testing.T) {
	initTestSecret(t)
	t.Setenv("FREEBUFF_CONFIG_DIR", t.TempDir())
	var polls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case codePath:
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("code request must not send Authorization, got %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"loginUrl":        "https://freebuff.example/signin?fingerprint=abc",
				"fingerprintHash": "hash-1",
				"expiresAt":       "2026-09-25T17:20:00.000Z",
			})
		case statusPath:
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user": map[string]any{
					"id": "user-1", "name": nil, "email": "dev@example.com",
					"authToken": "token-1", "fingerprintId": r.URL.Query().Get("fingerprintId"),
					"fingerprintHash": "hash-1",
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.WebBaseURL = server.URL
	client.HTTPClient = server.Client()
	// Speed the poll loop up for the test.
	client.Sleep = func(ctx context.Context, d time.Duration) error { return nil }

	manager := NewLoginManager(client)
	flow, err := manager.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if flow.LoginURL == "" || flow.FingerprintHash != "hash-1" || flow.expiresAt != "2026-09-25T17:20:00.000Z" {
		t.Fatalf("flow = %+v", flow)
	}
	waitForStatus(t, flow, "succeeded")

	user, err := loadUser()
	if err != nil {
		t.Fatal(err)
	}
	if user.AuthToken != "token-1" || user.Email != "dev@example.com" || user.FingerprintHash != "hash-1" {
		t.Fatalf("user = %+v", user)
	}
}

// TestStatusSwallowsServerErrorsAndKeepsPolling pins the CLI rule that a 500 or
// a non-JSON body is not fatal: only a user object ends polling.
func TestStatusSwallowsServerErrorsAndKeepsPolling(t *testing.T) {
	initTestSecret(t)
	t.Setenv("FREEBUFF_CONFIG_DIR", t.TempDir())
	var polls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case codePath:
			_ = json.NewEncoder(w).Encode(map[string]any{"loginUrl": "https://freebuff.example/s", "fingerprintHash": "h", "expiresAt": "2026-01-01T00:00:00.000Z"})
		case statusPath:
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte("not json at all"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClient()
	client.WebBaseURL = server.URL
	client.HTTPClient = server.Client()
	user, err := client.Status(context.Background(), "fp", "h", "2026-01-01T00:00:00.000Z")
	if err != nil || user != nil {
		t.Fatalf("500 must be treated as pending, got user=%+v err=%v", user, err)
	}
	user, err = client.Status(context.Background(), "fp", "h", "2026-01-01T00:00:00.000Z")
	if err != nil || user != nil {
		t.Fatalf("non-JSON 200 must be treated as pending, got user=%+v err=%v", user, err)
	}
}

// TestCodeFailsLoudlyOnEmptyResponse pins the CLI rule that /code is a hard
// error when it does not return a usable loginUrl.
func TestCodeFailsLoudlyOnEmptyResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"fingerprintHash": "h"})
	}))
	defer server.Close()
	client := NewClient()
	client.WebBaseURL = server.URL
	client.HTTPClient = server.Client()
	if _, err := client.Code(context.Background(), "fp"); err == nil {
		t.Fatal("expected error when loginUrl is missing")
	}
}

// TestPollTimeoutIsFiveMinutes pins the 5-minute client timeout, which is
// deliberately shorter than the server's one-hour code lifetime.
func TestPollTimeoutIsFiveMinutes(t *testing.T) {
	if PollTimeout != 5*time.Minute {
		t.Fatalf("PollTimeout = %s, want 5m", PollTimeout)
	}
	initTestSecret(t)
	t.Setenv("FREEBUFF_CONFIG_DIR", t.TempDir())
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codePath {
			_ = json.NewEncoder(w).Encode(map[string]any{"loginUrl": "https://freebuff.example/s", "fingerprintHash": "h", "expiresAt": "x"})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client := NewClient()
	client.WebBaseURL = server.URL
	client.HTTPClient = server.Client()
	// A fake clock that jumps past the timeout on the first poll.
	now := time.Now()
	client.Now = func() time.Time { return now }
	client.Sleep = func(ctx context.Context, d time.Duration) error {
		now = now.Add(time.Minute)
		return nil
	}
	manager := NewLoginManager(client)
	flow, err := manager.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, flow, "failed")
	if status, message := flow.StatusSnapshot(); status != "failed" || message == "" {
		t.Fatalf("status=%s message=%s", status, message)
	}
}

// TestLoginReusesImportedFingerprint ensures an existing CLI login keeps using
// the same device id rather than registering a second device.
func TestLoginReusesImportedFingerprint(t *testing.T) {
	initTestSecret(t)
	dir := t.TempDir()
	t.Setenv("FREEBUFF_CONFIG_DIR", dir)
	if err := writeCLICredentials(dir, "fp-existing", "token-existing"); err != nil {
		t.Fatal(err)
	}
	var seen atomic.Value
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codePath {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			seen.Store(body["fingerprintId"])
			_ = json.NewEncoder(w).Encode(map[string]any{"loginUrl": "https://freebuff.example/s", "fingerprintHash": "h", "expiresAt": "x"})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client := NewClient()
	client.WebBaseURL = server.URL
	client.HTTPClient = server.Client()
	client.Sleep = func(ctx context.Context, d time.Duration) error { return nil }
	manager := NewLoginManager(client)
	flow, err := manager.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if flow.FingerprintID != "fp-existing" {
		t.Fatalf("fingerprintID = %q, want imported fp-existing", flow.FingerprintID)
	}
	if got, _ := seen.Load().(string); got != "fp-existing" {
		t.Fatalf("server saw fingerprintId %q", got)
	}
}

// TestCodeAcceptsNumericExpiresAt pins the live wire format. The Freebuff TS
// types declare expiresAt a string, but the production endpoint returns
// epoch-millis as a JSON number; decoding it into a string field would fail the
// whole response and break login entirely.
func TestCodeAcceptsNumericExpiresAt(t *testing.T) {
	initTestSecret(t)
	t.Setenv("FREEBUFF_CONFIG_DIR", t.TempDir())
	const numericExpiresAt = "1790360296254"
	var echoed atomic.Value
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case codePath:
			// Shaped exactly like the live response.
			_, _ = w.Write([]byte(`{"fingerprintId":"fp","fingerprintHash":"2a8ee491","loginUrl":"https://freebuff.com/authorize?x=1","expiresAt":` + numericExpiresAt + `,"expiresInMs":3600000}`))
		case statusPath:
			// The numeric value must be echoed verbatim, not reformatted.
			echoed.Store(r.URL.Query().Get("expiresAt"))
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClient()
	client.WebBaseURL = server.URL
	client.HTTPClient = server.Client()
	client.Sleep = func(ctx context.Context, d time.Duration) error { return nil }
	manager := NewLoginManager(client)
	flow, err := manager.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Cancel(flow.ID) }()
	if got, _ := waitForValue(t, &echoed); got != numericExpiresAt {
		t.Fatalf("expiresAt echoed as %q, want the raw %q", got, numericExpiresAt)
	}
}

// TestCodeAcceptsStringExpiresAt keeps the string form working in case a
// deployment (or a self-hosted mirror) uses ISO strings as the types suggest.
func TestCodeAcceptsStringExpiresAt(t *testing.T) {
	initTestSecret(t)
	t.Setenv("FREEBUFF_CONFIG_DIR", t.TempDir())
	var echoed atomic.Value
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codePath {
			_, _ = w.Write([]byte(`{"loginUrl":"https://freebuff.com/x","fingerprintHash":"h","expiresAt":"2026-09-25T17:20:00.000+00:00"}`))
			return
		}
		echoed.Store(r.URL.Query().Get("expiresAt"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client := NewClient()
	client.WebBaseURL = server.URL
	client.HTTPClient = server.Client()
	client.Sleep = func(ctx context.Context, d time.Duration) error { return nil }
	manager := NewLoginManager(client)
	flow, err := manager.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Cancel(flow.ID) }()
	if got, _ := waitForValue(t, &echoed); got != "2026-09-25T17:20:00.000+00:00" {
		t.Fatalf("expiresAt echoed as %q", got)
	}
}

// waitForValue blocks until a captured atomic.Value has been populated.
func waitForValue(t *testing.T, slot *atomic.Value) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if value, ok := slot.Load().(string); ok {
			return value, true
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("server was never called")
	return "", false
}

func waitForStatus(t *testing.T, flow *LoginFlow, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if status, _ := flow.StatusSnapshot(); status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	status, message := flow.StatusSnapshot()
	t.Fatalf("flow status = %q (%s), want %q", status, message, want)
}
