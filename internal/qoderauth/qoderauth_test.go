package qoderauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
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
	for _, key := range []string{keyAccessToken, keyRefreshToken, keyExpiresAt, keyMachineID, keyUID, keyNickname} {
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

// The token arrives under different names by endpoint version, and identity
// arrives as user_id/user_name: all three token spellings must authenticate,
// and the uid must be kept, because inference signing refuses an empty one.
func TestPollReadsAlternateTokenFieldsAndIdentity(t *testing.T) {
	isolatedStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_token":  "tok_device",
			"refresh_token": "tok_refresh",
			"expire_time":   1780000000000,
			"user_id":       "u-123",
			"user_name":     "Emperor",
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
	if got, _ := secret.Get(keyAccessToken); got != "tok_device" {
		t.Errorf("stored access token = %q, want tok_device", got)
	}
	if got := UID(); got != "u-123" {
		t.Errorf("uid = %q, want u-123", got)
	}
	if got := Nickname(); got != "Emperor" {
		t.Errorf("nickname = %q, want Emperor", got)
	}
	// 1780000000000 ms is 1780000000 s.
	if got, _ := secret.Get(keyExpiresAt); got != "1780000000" {
		t.Errorf("stored expiry = %q, want 1780000000", got)
	}
}

// The refresh body is JSON carrying this machine's id with the vendor's
// User-Agent, not a form-encoded standard grant: the endpoint binds the grant
// to the polling machine.
func TestRefreshSendsJSONWithMachineID(t *testing.T) {
	isolatedStore(t)
	mid, err := machineID()
	if err != nil {
		t.Fatal(err)
	}
	var gotBody []byte
	var gotUA, gotCT string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotUA, gotCT = r.Header.Get("User-Agent"), r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"tok_new","refresh_token":"tok_refresh2","expires_in":3600}`))
	}))
	defer server.Close()
	if err := secret.Set(keyRefreshToken, "tok_refresh"); err != nil {
		t.Fatal(err)
	}

	mgr := NewManagerWithOptions(
		Endpoints{DeviceSelect: "https://qoder.com", OpenAPI: server.URL},
		"test-client",
		&http.Client{Timeout: 10 * time.Second},
	)
	if _, err := mgr.RefreshToken(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("refresh body is not JSON: %q", gotBody)
	}
	if body["refresh_token"] != "tok_refresh" || body["machine_id"] != mid {
		t.Errorf("refresh body = %v, want refresh_token and this machine_id", body)
	}
	if _, ok := body["grant_type"]; ok {
		t.Errorf("refresh body carries grant_type: this is not a standard grant: %v", body)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotCT)
	}
	if gotUA != "qoder/1.0.0" {
		t.Errorf("user-agent = %q, want qoder/1.0.0", gotUA)
	}
	if got, _ := secret.Get(keyAccessToken); got != "tok_new" {
		t.Errorf("stored access token = %q, want tok_new", got)
	}
}

// A refresh that omits identity must not wipe it: uid and nickname established
// at login stay until a response replaces them, or the signer loses the uid it
// needs on the very next request.
func TestRefreshPreservesIdentityItDoesNotReplace(t *testing.T) {
	isolatedStore(t)
	for key, value := range map[string]string{keyAccessToken: "tok", keyRefreshToken: "tok_refresh", keyUID: "u-123", keyNickname: "Emperor"} {
		if err := secret.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"tok_new2","expires_in":3600}`))
	}))
	defer server.Close()

	mgr := NewManagerWithOptions(
		Endpoints{DeviceSelect: "https://qoder.com", OpenAPI: server.URL},
		"test-client",
		&http.Client{Timeout: 10 * time.Second},
	)
	if _, err := mgr.RefreshToken(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := UID(); got != "u-123" {
		t.Errorf("uid after refresh = %q, want the login value kept", got)
	}
	if got := Nickname(); got != "Emperor" {
		t.Errorf("nickname after refresh = %q, want the login value kept", got)
	}
}
