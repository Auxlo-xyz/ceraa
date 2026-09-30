package qoderauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/internal/secret"
)

// isolatedStore points the shared credential store at a fresh temp directory.
//
// The store is process-global and persists across tests, so without this a token
// stored by one test is visible to the next. That is exactly how a refresh test
// once ran against a token planted by the success test and hit the real server.
func isolatedStore(t *testing.T) {
	t.Helper()
	t.Setenv("CERAA_HOME", t.TempDir())
	dir := os.Getenv("CERAA_HOME")
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
}

// The PKCE construction has to match the vendor's exactly, because the approval
// page checks the challenge and the poll checks the verifier. Established from
// the vendor bundle, not from the RFC, since a standard implementation and the
// vendor's differ in the alphabet length range.
func TestTheChallengeIsTheSHADigestOfTheVerifier(t *testing.T) {
	pair, err := newPKCEPair()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(pair.verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if pair.challenge != want {
		t.Errorf("challenge = %q, want base64url(sha256(verifier)) = %q", pair.challenge, want)
	}
	// 43 to 128 characters, the range in the bundle.
	if len(pair.verifier) < 43 || len(pair.verifier) > 128 {
		t.Errorf("verifier is %d chars, want 43-128", len(pair.verifier))
	}
	// The challenge must be URL-safe with no padding, or it breaks the query.
	if strings.ContainsAny(pair.challenge, "+/=") {
		t.Errorf("challenge = %q, want base64url without +/= ", pair.challenge)
	}
}

// The approval URL carries challenge, S256, nonce, machine id and client id,
// and nothing else the server needs. The client id is the CLI's, which differs
// from the web page's and is the one the log never showed.
func TestStartBuildsTheApprovalURLTheVendorExpects(t *testing.T) {
	isolatedStore(t)

	mgr := NewManagerWithOptions(Endpoints{DeviceSelect: "https://qoder.com", OpenAPI: "https://api.example"}, "test-client", &http.Client{})
	flow, err := mgr.Start()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(flow.Snapshot().AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme+"://"+parsed.Host != "https://qoder.com" || parsed.Path != "/device/selectAccounts" {
		t.Errorf("auth URL = %q, want qoder.com/device/selectAccounts", flow.Snapshot().AuthURL)
	}
	q := parsed.Query()
	for _, key := range []string{"challenge", "nonce", "machine_id", "client_id"} {
		if q.Get(key) == "" {
			t.Errorf("auth URL is missing %q: %s", key, flow.Snapshot().AuthURL)
		}
	}
	if q.Get("challenge_method") != "S256" {
		t.Errorf("challenge_method = %q, want S256", q.Get("challenge_method"))
	}
	if q.Get("client_id") != "test-client" {
		t.Errorf("client_id = %q, want the manager's client", q.Get("client_id"))
	}
	// The default is the CLI client from the bundle, not the web one from the
	// login page. Using the web id would authenticate as the wrong client.
	if defaultClientID != "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb" {
		t.Errorf("default client = %q, which is not the id from a working approval URL", defaultClientID)
	}
}

// The pending signal is an HTTP 404, not a JSON status. A reader that treats any
// non-2xx as a failure reports "declined" on the very first poll.
func TestAFourOhFourMeansPendingNotFailure(t *testing.T) {
	isolatedStore(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("nonce") == "" || q.Get("verifier") == "" || q.Get("challenge_method") != "S256" {
			t.Errorf("poll = %q, want nonce, verifier and S256", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	mgr := NewManagerWithOptions(
		Endpoints{DeviceSelect: "https://qoder.com", OpenAPI: server.URL},
		"test-client",
		&http.Client{Timeout: 10 * time.Second},
	)
	mgr.pollInterval = time.Millisecond
	mgr.deadline = 5 * time.Millisecond
	flow, err := mgr.Start()
	if err != nil {
		t.Fatal(err)
	}
	err = mgr.Poll(t.Context(), flow)
	if !IsPending(err) && err == nil {
		t.Fatalf("a 404-only poll should still be pending, got %v", err)
	}
	if snap := flow.Snapshot(); snap.Status != "pending" && snap.Status != "expired" {
		t.Errorf("flow status = %q after unapproved polls", snap.Status)
	}
}

// A successful poll returns the whole body and the token field is a string.
// Anything else is rejected rather than accepted on hope.
func TestASuccessfulPollStoresTheToken(t *testing.T) {
	isolatedStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":         "tok_live",
			"refresh_token": "tok_refresh",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	mgr := NewManagerWithOptions(
		Endpoints{DeviceSelect: "https://qoder.com", OpenAPI: server.URL},
		"test-client",
		&http.Client{Timeout: 10 * time.Second},
	)
	flow, err := mgr.Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Poll(t.Context(), flow); err != nil {
		t.Fatalf("a successful poll reported %v", err)
	}
	if got, err := secret.Get(keyAccessToken); err != nil || got != "tok_live" {
		t.Errorf("stored access token = %q, %v", got, err)
	}
	if got, err := secret.Get(keyRefreshToken); err != nil || got != "tok_refresh" {
		t.Errorf("stored refresh token = %q, %v", got, err)
	}
	// Namespaced, or one variable would wear two vendors' names.
	for _, key := range []string{keyAccessToken, keyRefreshToken, keyExpiresAt, keyMachineID} {
		if !strings.HasPrefix(key, "qoder_") {
			t.Errorf("secret key %q is not namespaced", key)
		}
	}
}

// A token field that is not a string is a failure, not a slow yes. The vendor's
// own parser enforces exactly this.
func TestANonStringTokenIsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token": 42}`))
	}))
	defer server.Close()
	mgr := NewManagerWithOptions(
		Endpoints{DeviceSelect: "https://qoder.com", OpenAPI: server.URL},
		"test-client",
		&http.Client{Timeout: 10 * time.Second},
	)
	flow, err := mgr.Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Poll(t.Context(), flow); err == nil {
		t.Fatal("a numeric token was accepted")
	}
}

// A missing refresh token is a signal, not silence: without it the integration
// dies on expiry, so the absence has to surface as an error the user can act on.
func TestARefreshWithoutAStoredTokenSaysSo(t *testing.T) {
	isolatedStore(t)

	if _, err := NewManager().RefreshToken(t.Context()); err == nil {
		t.Fatal("refresh with no stored token reported success")
	} else if !strings.Contains(err.Error(), "sign in again") {
		t.Errorf("error = %q, want it to say to sign in", err)
	}
}

// The machine id is Ceraa's own, generated and persisted once. Reusing the
// vendor CLI's would bind two different authorizations to one identifier.
func TestTheMachineIDIsStableAndIsNotTheVendors(t *testing.T) {
	isolatedStore(t)
	first, err := machineID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := machineID()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("machine id changed between reads: %q vs %q", first, second)
	}
	if first == "6d9ee62b-bbd2-4744-8906-22bd1e4d39dc" {
		t.Error("the machine id is the vendor CLI's, not Ceraa's own")
	}
}

// Snapshot deadlocked once, because it held the lock and called a method that
// takes it. Go mutexes are not reentrant. A deadlock cannot be asserted
// directly without risking the test itself, so this fails on slowness instead.
func TestSnapshotDoesNotDeadlock(t *testing.T) {
	isolatedStore(t)
	mgr := NewManagerWithOptions(Endpoints{DeviceSelect: "https://qoder.com", OpenAPI: "https://api.example"}, "test-client", &http.Client{Timeout: 10 * time.Second})
	flow, err := mgr.Start()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan State, 1)
	go func() { done <- flow.Snapshot() }()
	select {
	case snap := <-done:
		if snap.AuthURL == "" || snap.Nonce == "" {
			t.Errorf("snapshot = %+v, missing the flow's own fields", snap)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Snapshot did not return in 5s; a method it calls is probably taking the lock it holds")
	}
}
