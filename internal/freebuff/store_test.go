package freebuff

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestStoreStatusClearsCredentialOnAuthFailure pins the rule that a 401 on
// /api/v1/me clears the stored credential so the user is told to log in again.
func TestStoreStatusClearsCredentialOnAuthFailure(t *testing.T) {
	initTestSecret(t)
	t.Setenv("FREEBUFF_CONFIG_DIR", t.TempDir())
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	if err := saveUser(User{AuthToken: "stale", FingerprintID: "fp", Email: "dev@example.com"}); err != nil {
		t.Fatal(err)
	}
	store := &Store{APIBaseURL: server.URL, Client: newTestClient(server)}
	identity, err := store.Status(context.Background())
	if err == nil {
		t.Fatal("expected an auth error")
	}
	if identity != "dev@example.com" {
		t.Fatalf("identity = %q", identity)
	}
	if _, loadErr := loadUser(); !errors.Is(loadErr, ErrNoCredential) {
		t.Fatalf("credential should have been cleared, got %v", loadErr)
	}
}

// TestStoreStatusKeepsCredentialOnTransientError pins the rule that a 5xx must
// not log the user out.
func TestStoreStatusKeepsCredentialOnTransientError(t *testing.T) {
	initTestSecret(t)
	t.Setenv("FREEBUFF_CONFIG_DIR", t.TempDir())
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	if err := saveUser(User{AuthToken: "good", FingerprintID: "fp", Email: "dev@example.com"}); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(server)
	client.Sleep = func(ctx context.Context, d time.Duration) error { return nil }
	store := &Store{APIBaseURL: server.URL, Client: client}
	if _, err := store.Status(context.Background()); err == nil {
		t.Fatal("expected a transient error to be reported")
	}
	user, loadErr := loadUser()
	if loadErr != nil {
		t.Fatalf("credential must survive a transient error: %v", loadErr)
	}
	if user.AuthToken != "good" {
		t.Fatalf("token = %q", user.AuthToken)
	}
}

// TestStoreLogoutIsIdempotent covers logging out twice and logging out when
// never logged in.
func TestStoreLogoutIsIdempotent(t *testing.T) {
	initTestSecret(t)
	t.Setenv("FREEBUFF_CONFIG_DIR", t.TempDir())
	store := NewStore()
	if err := store.Logout(context.Background()); err != nil {
		t.Fatalf("logout with no credential must succeed: %v", err)
	}
	if err := saveUser(User{AuthToken: "t", FingerprintID: "fp"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := loadUser(); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("credential should be gone, got %v", err)
	}
}

// TestStoreResolveUsesHostedInferenceBase pins the inference base: Ceraa's
// transport appends /chat/completions, so the base must include /api/v1.
func TestStoreResolveUsesHostedInferenceBase(t *testing.T) {
	initTestSecret(t)
	t.Setenv("FREEBUFF_CONFIG_DIR", t.TempDir())
	if err := saveUser(User{AuthToken: "t", FingerprintID: "fp", Email: "dev@example.com"}); err != nil {
		t.Fatal(err)
	}
	credential, err := NewStore().Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if credential.BaseURL != "https://www.codebuff.com/api/v1" {
		t.Fatalf("BaseURL = %q", credential.BaseURL)
	}
	if credential.AccessToken != "t" || credential.Source != "freebuff-native" {
		t.Fatalf("credential = %+v", credential)
	}
	if credential.BaseURL+"/chat/completions" != FreebuffCompletionsURL() {
		t.Fatalf("base + /chat/completions = %q, want %q", credential.BaseURL+"/chat/completions", FreebuffCompletionsURL())
	}
}

// TestStatusWithoutUserIsPending pins that a 200 with no user object keeps the
// login flow waiting rather than failing it.
func TestStatusWithoutUserIsPending(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()
	user, err := newTestClient(server).Status(context.Background(), "fp", "h", "x")
	if err != nil || user != nil {
		t.Fatalf("user=%+v err=%v", user, err)
	}
}
