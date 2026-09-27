package clineauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ceraa/internal/oauthdevice"
)

func initSecret(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := secretInit(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}
}

// Cline publishes the client id rather than keeping it private, which is what
// makes this wireable at all. Compare with a provider that does not.
func TestClientIDIsPublished(t *testing.T) {
	// Pinned exactly. A prefix check passed a wrong id, and a wrong id is a login
	// that cannot work while looking correct.
	if WorkOSClientID != "client_01K3A541FN8TA3EPPHTD2325AR" {
		t.Errorf("client id = %q", WorkOSClientID)
	}
	if WorkOSAPIBase != "https://api.workos.com" {
		t.Errorf("identity base = %q", WorkOSAPIBase)
	}
	// The device code must never be sent to the gateway host.
	endpoints := DeviceEndpoints()
	if !strings.HasPrefix(endpoints.DeviceCode, WorkOSAPIBase) {
		t.Errorf("the device code would be sent to %q, not the identity provider", endpoints.DeviceCode)
	}
	if !strings.HasPrefix(endpoints.Token, WorkOSAPIBase) {
		t.Errorf("the token call would go to %q, not the identity provider", endpoints.Token)
	}
}

func TestSignedInReflectsTheStoredCredential(t *testing.T) {
	initSecret(t)
	if SignedIn() {
		t.Error("SignedIn with nothing stored")
	}
	if err := Save(Credential{AccessToken: "acc-1", RefreshToken: "ref-1"}); err != nil {
		t.Fatal(err)
	}
	if !SignedIn() {
		t.Error("SignedIn false after storing a credential")
	}
	if err := Save(Credential{RefreshToken: "ref-1"}); err == nil {
		t.Error("a credential with no access token was stored; it would read as signed in and fail on first use")
	}
}

func TestLoadRejectsATokenlessRecord(t *testing.T) {
	initSecret(t)
	if err := secretSet(secretName, `{"refresh_token":"ref-1"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Error("a tokenless record was accepted; the menu would offer a switch that fails")
	}
	if SignedIn() {
		t.Error("SignedIn true for a tokenless record")
	}
}

// The device flow and the persistence of its token, driven end to end.
func TestDeviceLoginPersistsTheToken(t *testing.T) {
	initSecret(t)
	var polls int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/authorize/device"):
			_, _ = w.Write([]byte(`{"device_code":"d1","user_code":"U-1","verification_uri":"https://authkit.cline.bot/device","verification_uri_complete":"https://authkit.cline.bot/device?user_code=U-1","expires_in":300,"interval":1}`))
		default:
			polls++
			if polls < 2 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"acc-c","refresh_token":"ref-c","token_type":"Bearer","expires_in":3600}`))
		}
	}))
	defer srv.Close()

	login := NewLoginWithEndpoints(srv.Client(), stubEndpoints(srv))
	flow, err := login.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.After(20 * time.Second)
	for flow.Snapshot().Status != oauthdevice.StatusSucceeded {
		select {
		case <-deadline:
			t.Fatalf("login did not complete, status %q", flow.Snapshot().Status)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !SignedIn() {
		t.Fatal("the login succeeded but nothing was stored")
	}
	credential, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "acc-c" || credential.RefreshToken != "ref-c" {
		t.Errorf("stored credential = %+v", credential)
	}
	if credential.ExpiresAt.IsZero() {
		t.Error("expiry was not recorded, so the token can never be refreshed")
	}
}

// A pending poll must stay pending. This is the case a browser-tab bridge cannot
// express at all, and the one that makes a headless bot possible.
func TestPendingLoginDoesNotComplete(t *testing.T) {
	initSecret(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/authorize/device") {
			_, _ = w.Write([]byte(`{"device_code":"d2","user_code":"U-2","verification_uri":"https://authkit.cline.bot/device","expires_in":300,"interval":1}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	defer srv.Close()

	login := NewLoginWithEndpoints(srv.Client(), stubEndpoints(srv))
	flow, err := login.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer login.Manager().Cancel(flow.ID)
	time.Sleep(200 * time.Millisecond)
	if got := flow.Snapshot().Status; got != oauthdevice.StatusPending {
		t.Errorf("status = %q, want pending", got)
	}
	if SignedIn() {
		t.Error("a pending login stored a credential")
	}
}

// The catalog needs no credential, which is how a user sees what signing in gets
// them before signing in.
func TestCatalogNeedsNoCredential(t *testing.T) {
	var authorization string
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"recommended":[{"id":"a/b","name":"B"}],"free":[{"id":"cline-free/x","name":"X"},{"id":"stealth/y","name":"Y"}],"clinePass":[],"clineCloud":[]}`))
	}))
	defer srv.Close()

	catalog, err := fetchCatalogFrom(context.Background(), srv.Client(), srv.URL+"/catalog")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if authorization != "" {
		t.Errorf("the catalog was fetched with a credential (%q); it must be readable before sign-in", authorization)
	}
	if path != "/catalog" {
		t.Errorf("requested %q", path)
	}
	if len(catalog.Free) != 2 {
		t.Errorf("free tier = %d models, want 2", len(catalog.Free))
	}
	if len(catalog.Recommended) != 1 {
		t.Errorf("recommended = %d, want 1", len(catalog.Recommended))
	}
}

// The refresh lives on Cline's own API, not the identity provider, and carries
// the refresh token as a bearer. Getting that wrong sends the credential to the
// wrong host or in the wrong field.
func TestRefreshUsesTheGatewayEndpoint(t *testing.T) {
	initSecret(t)
	var gotPath, gotAuth, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		w.Header().Set("Content-Type", "application/json")
		// No refresh_token in the response, which is the case under test.
		_, _ = w.Write([]byte(`{"access_token":"acc-2","token_type":"Bearer"}`))
	}))
	defer srv.Close()

	if err := Save(Credential{AccessToken: "old", RefreshToken: "ref-1", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	store.HTTPClient = srv.Client()
	store.RefreshEndpoint = srv.URL + "/api/v1/auth/refresh"
	credential, err := Resolve(context.Background(), store)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("refresh used %s", gotMethod)
	}
	// The host matters as much as the path: WorkOS's own path could satisfy a
	// suffix check, and the refresh belongs on Cline's API.
	if gotPath != "/api/v1/auth/refresh" {
		t.Errorf("refresh path = %q, want Cline's own /api/v1/auth/refresh", gotPath)
	}
	if gotAuth != "Bearer ref-1" {
		t.Errorf("refresh sent %q, want the refresh token as a bearer", gotAuth)
	}
	if credential.AccessToken != "acc-2" {
		t.Errorf("access token = %q", credential.AccessToken)
	}
	if credential.RefreshToken != "ref-1" {
		t.Errorf("refresh token = %q; it must be kept when the response omits it", credential.RefreshToken)
	}
	// An assumed lifetime, so the token is refreshed on the skew rather than
	// being treated as never expiring.
	if credential.ExpiresAt.IsZero() {
		t.Error("the refreshed token has no expiry, so it would never be refreshed")
	}
}

func TestExpiredWithoutRefreshTokenIsReported(t *testing.T) {
	initSecret(t)
	if err := Save(Credential{AccessToken: "a", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(context.Background(), NewStore()); err == nil {
		t.Error("an expired credential with no refresh token resolved silently")
	}
}

func TestCancelledLoginReports(t *testing.T) {
	initSecret(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/authorize/device") {
			_, _ = w.Write([]byte(`{"device_code":"d3","user_code":"U-3","verification_uri":"https://authkit.cline.bot/device","expires_in":300,"interval":1}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	defer srv.Close()

	login := NewLoginWithEndpoints(srv.Client(), stubEndpoints(srv))
	flow, err := login.Start(context.Background())
	if err != nil {
		t.Fatal(err)
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

// The default refresh endpoint is what production uses, and a test that overrides
// it proves nothing about it. Pinned to Cline's own API, because the identity
// provider is a different host and a refresh sent there hands it a credential it
// has no use for.
func TestDefaultRefreshEndpointIsOnCline(t *testing.T) {
	endpoint := defaultRefreshEndpoint()
	if endpoint != "https://api.cline.bot/api/v1/auth/refresh" {
		t.Errorf("default refresh endpoint = %q", endpoint)
	}
	if !strings.HasPrefix(endpoint, "https://api.cline.bot") {
		t.Errorf("the refresh would leave Cline's API: %q", endpoint)
	}
	if strings.HasPrefix(endpoint, WorkOSAPIBase) {
		t.Errorf("the refresh is pointed at the identity provider: %q", endpoint)
	}
}

// The device flow and the refresh are on different hosts, and mixing them up is
// the mistake this pair of tests exists to catch.
func TestIdentityAndRefreshHostsDiffer(t *testing.T) {
	refresh := defaultRefreshEndpoint()
	for _, endpoint := range []string{DeviceEndpoints().DeviceCode, DeviceEndpoints().Token} {
		if strings.HasPrefix(refresh, strings.SplitN(endpoint, "/", 4)[2]) {
			t.Errorf("the refresh and the device flow share a host: %q vs %q", refresh, endpoint)
		}
	}
}
