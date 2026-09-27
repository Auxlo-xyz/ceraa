package clineauth

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

// The device flow and the persistence of the registered account credential,
// driven end to end. The WorkOS tokens are identity proof, not gateway access:
// without the registration call the login would store a token Cline's own API
// rejects.
func TestDeviceLoginPersistsTheToken(t *testing.T) {
	initSecret(t)
	var polls int
	var registerBody map[string]string
	registerHeaders := map[string]string{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/authorize/device"):
			_, _ = w.Write([]byte(`{"device_code":"d1","user_code":"U-1","verification_uri":"https://authkit.cline.bot/device","verification_uri_complete":"https://authkit.cline.bot/device?user_code=U-1","expires_in":300,"interval":1}`))
		case strings.HasSuffix(r.URL.Path, "/auth/register"):
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			registerHeaders["Content-Type"] = r.Header.Get("Content-Type")
			registerHeaders["User-Agent"] = r.Header.Get("User-Agent")
			registerHeaders["X-CLIENT-TYPE"] = r.Header.Get("X-CLIENT-TYPE")
			registerHeaders["X-CLIENT-VERSION"] = r.Header.Get("X-CLIENT-VERSION")
			registerHeaders["X-CORE-VERSION"] = r.Header.Get("X-CORE-VERSION")
			registerHeaders["X-Task-ID"] = r.Header.Get("X-Task-ID")
			if err := json.NewDecoder(r.Body).Decode(&registerBody); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"success":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"data":{"accessToken":"acc-registered","refreshToken":"ref-registered","tokenType":"Bearer","expiresAt":"2030-01-01T00:00:00Z","userInfo":{"email":"user@example.com","clineUserId":"acct-1"}}}`))
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

	login := NewLoginWithRegistration(srv.Client(), stubEndpoints(srv), srv.URL+"/api/v1/auth/register")
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
	if registerBody["accessToken"] != "acc-c" || registerBody["refreshToken"] != "ref-c" {
		t.Fatalf("registered WorkOS tokens = %v; the identity proof was not exchanged", registerBody)
	}
	if registerHeaders["Content-Type"] != "application/json" {
		t.Errorf("registration content type = %q, want application/json", registerHeaders["Content-Type"])
	}
	if registerHeaders["User-Agent"] != "Cline/"+OfficialCLIVersion ||
		registerHeaders["X-CLIENT-TYPE"] != OfficialClientName ||
		registerHeaders["X-CLIENT-VERSION"] != OfficialCLIVersion ||
		registerHeaders["X-CORE-VERSION"] != OfficialCoreVersion {
		t.Errorf("registration identity = %v; the gateway routes on the official client", registerHeaders)
	}
	if registerHeaders["X-Task-ID"] == "" {
		t.Error("registration omitted X-Task-ID, so the exchange cannot be correlated with the login")
	}
	if !SignedIn() {
		t.Fatal("the login succeeded but nothing was stored")
	}
	credential, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "acc-registered" || credential.RefreshToken != "ref-registered" {
		t.Errorf("stored credential = %+v; the WorkOS token was stored instead of the account credential", credential)
	}
	if credential.Email != "user@example.com" || credential.AccountID != "acct-1" {
		t.Errorf("stored account = %+v; the registration response was not retained", credential)
	}
	if credential.ExpiresAt.IsZero() {
		t.Error("expiry was not recorded, so the token can never be refreshed")
	}
	if credential.APIKey() != "workos:acc-registered" {
		t.Errorf("gateway credential = %q; the gateway requires the workos marker", credential.APIKey())
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

// The refresh lives on Cline's own API as a JSON call carrying the account
// refresh token. The old form post with the refresh token as a bearer never
// matched Cline's refresh contract.
func TestRefreshUsesTheGatewayEndpoint(t *testing.T) {
	initSecret(t)
	var gotPath, gotContentType, gotMethod string
	var gotBody map[string]string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotContentType, gotMethod = r.URL.Path, r.Header.Get("Content-Type"), r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"accessToken":"acc-2","refreshToken":"ref-2","tokenType":"Bearer","expiresAt":"2030-01-01T00:00:00Z","userInfo":{"email":"new@example.com","clineUserId":"acct-2"}}}`))
	}))
	defer srv.Close()

	if err := Save(Credential{Kind: CredentialKindAccount, AccessToken: "old", RefreshToken: "ref-1", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
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
	if gotContentType != "application/json" {
		t.Errorf("refresh content type = %q, want application/json", gotContentType)
	}
	if gotBody["refreshToken"] != "ref-1" || gotBody["grantType"] != "refresh_token" {
		t.Errorf("refresh body = %v; the account refresh belongs in JSON fields", gotBody)
	}
	if credential.AccessToken != "acc-2" {
		t.Errorf("access token = %q", credential.AccessToken)
	}
	if credential.RefreshToken != "ref-2" {
		t.Errorf("refresh token = %q; the gateway-issued refresh must replace the old one", credential.RefreshToken)
	}
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

func TestSaveMarksNewCredentialsAsAccounts(t *testing.T) {
	initSecret(t)
	if err := Save(Credential{AccessToken: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	credential, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if credential.Kind != CredentialKindAccount {
		t.Errorf("stored kind = %q; a new credential must be distinguishable from a legacy identity token", credential.Kind)
	}
}

func TestAPIKeyAddsTheGatewayMarker(t *testing.T) {
	cases := map[string]string{
		"acc-1":        "workos:acc-1",
		"  acc-1  ":    "workos:acc-1",
		"workos:acc-1": "workos:acc-1",
		"WORKOS:acc-1": "WORKOS:acc-1",
	}
	for token, want := range cases {
		if got := (Credential{AccessToken: token}).APIKey(); got != want {
			t.Errorf("APIKey(%q) = %q, want %q", token, got, want)
		}
	}
}

func TestRegisterRejectsAnIncompleteAccountResponse(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"accessToken":"acc-1"}}`))
	}))
	defer srv.Close()

	if _, err := registerAccount(context.Background(), srv.Client(), srv.URL+"/api/v1/auth/register", nil, "workos-access", "workos-refresh"); err == nil {
		t.Error("an account response without the account refresh was accepted; the credential cannot be renewed")
	}
}

func TestRegisterRefusesPlainHTTP(t *testing.T) {
	if _, err := registerAccount(context.Background(), nil, "http://api.cline.bot/api/v1/auth/register", nil, "workos-access", "workos-refresh"); err == nil {
		t.Error("an account token would be exchanged over plain HTTP")
	}
}

// A credential stored before registration existed must be exchanged before it
// can be used. Sending its WorkOS access token to the gateway is exactly the
// request that was returning 401.
func TestLegacyStoredSignInIsRegisteredBeforeUse(t *testing.T) {
	initSecret(t)
	var gotBody map[string]string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/v1/auth/register") {
			t.Errorf("legacy upgrade requested %q; only registration may redeem it", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"accessToken":"acc-upgraded","refreshToken":"ref-upgraded","tokenType":"Bearer","expiresAt":"2030-01-01T00:00:00Z","userInfo":{"email":"legacy@example.com","clineUserId":"acct-legacy"}}}`))
	}))
	defer srv.Close()

	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := secretSet(secretName, `{"access_token":"workos-access-legacy","refresh_token":"workos-refresh-legacy","expires_at":"`+expiry+`"}`); err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	store.HTTPClient = srv.Client()
	store.RegisterEndpoint = srv.URL + "/api/v1/auth/register"
	credential, err := Resolve(context.Background(), store)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gotBody["accessToken"] != "workos-access-legacy" || gotBody["refreshToken"] != "workos-refresh-legacy" {
		t.Fatalf("registration body = %v; the legacy identity token was not exchanged", gotBody)
	}
	if credential.Kind != CredentialKindAccount || credential.AccessToken != "acc-upgraded" {
		t.Errorf("resolved credential = %+v; the raw identity token would still be sent", credential)
	}
	if stored, err := Load(); err != nil {
		t.Fatal(err)
	} else if stored.AccessToken != "acc-upgraded" || stored.Kind != CredentialKindAccount {
		t.Errorf("stored credential = %+v; the upgrade was not persisted", stored)
	}
}

// The default registration endpoint is what production uses, and an exchange
// sent to WorkOS instead would ask the identity provider for a credential only
// Cline can issue.
func TestDefaultRegisterEndpointIsOnCline(t *testing.T) {
	endpoint := defaultRegisterEndpoint()
	if endpoint != "https://api.cline.bot/api/v1/auth/register" {
		t.Errorf("default register endpoint = %q", endpoint)
	}
	if !strings.HasPrefix(endpoint, "https://api.cline.bot") {
		t.Errorf("the registration would leave Cline's API: %q", endpoint)
	}
	if strings.HasPrefix(endpoint, WorkOSAPIBase) {
		t.Errorf("the registration is pointed at the identity provider: %q", endpoint)
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
