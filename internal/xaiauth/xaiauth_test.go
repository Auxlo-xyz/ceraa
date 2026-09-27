package xaiauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ceraa/internal/oauthdevice"
	"ceraa/internal/secret"
)

func initSecret(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}
}

// The constants are the whole point of this package, and a typo in any of them
// makes sign-in fail at the provider rather than here. Pinned so the edit that
// breaks it is caught by a test.
func TestOAuthConstantsMatchTheIssuer(t *testing.T) {
	if Issuer != "https://auth.x.ai" {
		t.Errorf("issuer = %q", Issuer)
	}
	if ClientID != "b1a00492-073a-47ea-816f-4c329264a828" {
		t.Errorf("client id = %q", ClientID)
	}
	// offline_access is what makes xAI issue a refresh token. Without it the login
	// works and then dies an hour later with nothing to refresh.
	if !strings.Contains(Scope, "offline_access") {
		t.Error("scope lost offline_access, so no refresh token would be issued")
	}
	if !strings.Contains(Scope, "api:access") {
		t.Error("scope lost api:access, so inference would be refused")
	}
}

// SignedIn decides whether the menu offers a switch, so a record with no token
// must not read as signed in.
func TestSignedInReflectsTheStoredCredential(t *testing.T) {
	initSecret(t)
	if SignedIn() {
		t.Error("SignedIn() with nothing stored")
	}
	if err := Save(Credential{AccessToken: "acc-1", RefreshToken: "ref-1"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if !SignedIn() {
		t.Error("SignedIn() false after storing a credential")
	}
	credential, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if credential.BaseURL != InferenceBaseURL {
		t.Errorf("base URL = %q, want the Grok endpoint", credential.BaseURL)
	}
	if err := Save(Credential{RefreshToken: "ref-1"}); err == nil {
		t.Error("a credential with no access token was stored; it would read as signed in and fail on first use")
	}
}

// A device login must persist the token, or it reports success and then has
// nowhere to put the credential.
func TestDeviceLoginPersistsTheToken(t *testing.T) {
	initSecret(t)

	var polls int
	var mu sync.Mutex
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/device/code"):
			_, _ = w.Write([]byte(`{"device_code":"d1","user_code":"U-1","verification_uri":"https://accounts.x.ai/oauth2/device","verification_uri_complete":"https://accounts.x.ai/oauth2/device?user_code=U-1","expires_in":1800,"interval":1}`))
		case strings.HasSuffix(r.URL.Path, "/token"):
			mu.Lock()
			polls++
			first := polls == 1
			mu.Unlock()
			if first {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"acc-x","refresh_token":"ref-x","token_type":"Bearer","expires_in":7200,"scope":"` + Scope + `"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	login := NewLoginWithConfig(withEndpoints(ManagerConfig(),
		oauthdevice.Endpoints{DeviceCode: srv.URL + "/oauth2/device/code", Token: srv.URL + "/oauth2/token"}, srv.Client()))
	flow, err := login.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, func() bool { return flow.Snapshot().Status == oauthdevice.StatusSucceeded })

	if !SignedIn() {
		t.Fatal("the login succeeded but nothing was stored")
	}
	credential, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if credential.AccessToken != "acc-x" || credential.RefreshToken != "ref-x" {
		t.Errorf("stored credential = %+v", credential)
	}
	if credential.ExpiresAt.IsZero() {
		t.Error("expiry was not recorded, so the token can never be refreshed")
	}
}

// A login the user abandons has to report, or the chat waits forever.
func TestCancelledLoginReports(t *testing.T) {
	initSecret(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/device/code") {
			_, _ = w.Write([]byte(`{"device_code":"d2","user_code":"U-2","verification_uri":"https://accounts.x.ai/oauth2/device","expires_in":1800,"interval":1}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	defer srv.Close()

	login := NewLoginWithConfig(withEndpoints(ManagerConfig(),
		oauthdevice.Endpoints{DeviceCode: srv.URL + "/oauth2/device/code", Token: srv.URL + "/oauth2/token"}, srv.Client()))
	flow, err := login.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var mu sync.Mutex
	var terminal []oauthdevice.Snapshot
	login.Manager().SetOnComplete(func(snapshot oauthdevice.Snapshot) {
		mu.Lock()
		terminal = append(terminal, snapshot)
		mu.Unlock()
	})
	time.Sleep(150 * time.Millisecond)
	if err := login.Manager().Cancel(flow.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(terminal) != 1 || terminal[0].Status != oauthdevice.StatusCanceled {
		t.Errorf("terminal states = %+v, want one cancel", terminal)
	}
}

// A refresh response usually omits refresh_token, meaning "keep the one you
// have". Dropping it signs the user out at the next expiry.
func TestRefreshKeepsTheExistingRefreshToken(t *testing.T) {
	initSecret(t)
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// No refresh_token in the response, which is the case under test.
		_, _ = w.Write([]byte(`{"access_token":"acc-2","token_type":"Bearer","expires_in":7200}`))
	}))
	defer srv.Close()

	if err := Save(Credential{AccessToken: "acc-1", RefreshToken: "ref-1", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store := NewStore()
	store.HTTPClient = srv.Client()
	store.Endpoints = oauthdevice.Endpoints{DeviceCode: srv.URL + "/oauth2/device/code", Token: srv.URL + "/oauth2/token"}
	credential, err := Resolve(context.Background(), store)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if credential.AccessToken != "acc-2" {
		t.Errorf("access token = %q, want the refreshed one", credential.AccessToken)
	}
	if credential.RefreshToken != "ref-1" {
		t.Errorf("refresh token = %q; it must be kept when the response omits it", credential.RefreshToken)
	}
}

// An expired token with no refresh token must say so, rather than sending a
// credential that is about to 401.
func TestExpiredWithoutRefreshTokenIsReported(t *testing.T) {
	initSecret(t)
	if err := Save(Credential{AccessToken: "acc-1", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Resolve(context.Background(), NewStore()); err == nil {
		t.Error("an expired credential with no refresh token resolved silently")
	}
}

// Discovery can name any host, so endpoints off the issuer's origin are refused.
func TestDiscoveryRejectsAnOffOriginEndpoint(t *testing.T) {
	cases := []struct {
		name   string
		issuer string
		doc    string
	}{
		{
			"device endpoint on another host",
			"https://auth.x.ai",
			`{"issuer":"https://auth.x.ai","device_authorization_endpoint":"https://evil.example/device","token_endpoint":"https://auth.x.ai/oauth2/token"}`,
		},
		{
			"token endpoint on another host",
			"https://auth.x.ai",
			`{"issuer":"https://auth.x.ai","device_authorization_endpoint":"https://auth.x.ai/oauth2/device/code","token_endpoint":"https://evil.example/token"}`,
		},
		{
			"a different issuer entirely",
			"https://auth.x.ai",
			`{"issuer":"https://auth.x.ai","device_authorization_endpoint":"https://auth.x.ai.evil.example/device","token_endpoint":"https://auth.x.ai/oauth2/token"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.doc))
			}))
			defer srv.Close()
			if _, err := DiscoverFrom(context.Background(), srv.URL, srv.Client()); err == nil {
				t.Errorf("discovery accepted %s", tc.name)
			}
		})
	}
}

// The on-origin endpoints the real issuer publishes must still be accepted, or the
// check above is simply refusing everything.
func TestDiscoveryAcceptsOnOriginEndpoints(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issuer":"https://auth.x.ai","device_authorization_endpoint":"https://auth.x.ai/oauth2/device/code","token_endpoint":"https://auth.x.ai/oauth2/token"}`))
	}))
	defer srv.Close()
	document, err := DiscoverFrom(context.Background(), srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("on-origin endpoints were refused: %v", err)
	}
	if document.TokenEndpoint != "https://auth.x.ai/oauth2/token" {
		t.Errorf("token endpoint = %q", document.TokenEndpoint)
	}
}

// A record in the store with no access token must not read as signed in, even if
// it got there without going through Save.
func TestLoadRejectsATokenlessRecord(t *testing.T) {
	initSecret(t)
	if err := secret.Set(secretName(), `{"refresh_token":"ref-1","base_url":"https://api.x.ai/v1"}`); err != nil {
		t.Fatalf("plant record: %v", err)
	}
	if _, err := Load(); err == nil {
		t.Error("a tokenless record was accepted; the menu would offer a switch that fails")
	}
	if SignedIn() {
		t.Error("SignedIn() true for a tokenless record")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		if condition() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition was never met")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

var _ = json.Marshal
