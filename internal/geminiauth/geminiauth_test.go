package geminiauth

import (
	"encoding/base64"
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
// The store is process-global and persists across tests, so without this a
// token stored by one test is visible to the next.
func isolatedStore(t *testing.T) {
	t.Helper()
	t.Setenv("CERAA_HOME", t.TempDir())
	dir := os.Getenv("CERAA_HOME")
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
}

// The approval URL carries the client, the loopback redirect, the six scopes
// and a state to check the callback against. Google rejects anything else
// with redirect_uri_mismatch or a missing-secret refusal, both of which read
// as a broken login rather than a malformed request.
func TestStartBuildsTheApprovalURLGoogleExpects(t *testing.T) {
	flow, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(flow.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme+"://"+parsed.Host != "https://accounts.google.com" || parsed.Path != "/o/oauth2/v2/auth" {
		t.Errorf("auth URL = %q, want the Google OAuth endpoint", flow.AuthorizeURL)
	}
	q := parsed.Query()
	if q.Get("client_id") != DefaultClientID {
		t.Errorf("client_id = %q, want the public client", q.Get("client_id"))
	}
	if q.Get("response_type") != "code" || q.Get("access_type") != "offline" || q.Get("prompt") != "consent" {
		t.Errorf("query = %q, want code + offline + consent", parsed.RawQuery)
	}
	if q.Get("redirect_uri") != RedirectURI() {
		t.Errorf("redirect_uri = %q, want the loopback callback", q.Get("redirect_uri"))
	}
	if !strings.Contains(q.Get("scope"), "https://www.googleapis.com/auth/cloud-platform") {
		t.Errorf("scope = %q, want the six scopes including cloud-platform", q.Get("scope"))
	}
	if q.Get("state") == "" {
		t.Error("no state: the callback cannot be checked against this attempt")
	}
	if strings.TrimSpace(flow.State) == "" || q.Get("state") != flow.State {
		t.Error("flow state and URL state disagree")
	}
}

// A callback answering a different attempt is rejected, not exchanged: the
// state is the only proof the pasted URL answers this login.
func TestExchangeRejectsAForeignState(t *testing.T) {
	isolatedStore(t)
	mgr := NewManagerWithOptions("https://api.example", &http.Client{})
	flow := &Flow{State: "ours", RedirectURI: RedirectURI(), ExpiresAt: time.Now().Add(FlowExpiry)}
	pasted := "http://localhost:8845/oauth-callback?code=abc&state=theirs"
	if err := mgr.Exchange(t.Context(), flow, pasted); err == nil {
		t.Fatal("a foreign state was accepted")
	} else if !strings.Contains(err.Error(), "different login") {
		t.Errorf("error = %q, want it to name the mismatch", err)
	}
}

// A bare code carries no state to check, so it exchanges without one rather
// than failing a careful paste.
func TestExchangeAcceptsABareCode(t *testing.T) {
	isolatedStore(t)
	var gotForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			gotForm, _ = url.ParseQuery(string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok_live","refresh_token":"tok_refresh","expires_in":3600,"id_token":"x"}`))
	}))
	defer server.Close()

	mgr := NewManagerWithOptions(server.URL, &http.Client{}).SetUserinfoEndpoint(server.URL)
	flow := &Flow{State: "ours", RedirectURI: RedirectURI(), ExpiresAt: time.Now().Add(FlowExpiry)}
	if err := mgr.Exchange(t.Context(), flow, "bare-code-123"); err != nil {
		t.Fatalf("a bare code was rejected: %v", err)
	}
	if gotForm.Get("code") != "bare-code-123" {
		t.Errorf("code = %q", gotForm.Get("code"))
	}
	// The secret travels on every token call: without it Google answers
	// invalid_request while the browser showed success.
	if gotForm.Get("client_secret") != DefaultClientSecret {
		t.Errorf("client_secret missing: Google rejects the exchange without it")
	}
	if gotForm.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q", gotForm.Get("grant_type"))
	}
	if got, err := secret.Get(keyAccessToken); err != nil || got != "tok_live" {
		t.Errorf("stored access token = %q, %v", got, err)
	}
}

// A missing access token is a failure, not a slow yes; an error field is
// surfaced with its description rather than a bare status.
func TestGrantRefusalsAreNamed(t *testing.T) {
	for body, want := range map[string]string{
		`{"access_token":""}`: "no access token",
		`{"error":"invalid_grant","error_description":"bad"}`: "invalid_grant: bad",
		`not json`: "unreadably",
	} {
		if _, err := parseGrant([]byte(body), ""); err == nil {
			t.Errorf("%q accepted", body)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("%q error = %q, want %q", body, err, want)
		}
	}
}

// A refresh response without a new refresh token keeps the stored one:
// Google rotates occasionally, and overwriting with empty is how a working
// login dies overnight.
func TestRefreshKeepsTheStoredTokenWhenUnrotated(t *testing.T) {
	isolatedStore(t)
	if err := secret.Set(keyRefreshToken, "tok_old_refresh"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok_new","expires_in":3600}`))
	}))
	defer server.Close()

	mgr := NewManagerWithOptions(server.URL, &http.Client{})
	if _, err := mgr.RefreshToken(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got, _ := secret.Get(keyRefreshToken); got != "tok_old_refresh" {
		t.Errorf("refresh token = %q, want the stored one kept", got)
	}
	if got, _ := secret.Get(keyAccessToken); got != "tok_new" {
		t.Errorf("access token = %q, want tok_new", got)
	}
}

// Identity comes from the id_token without a network round trip, and a
// refresh that omits it must not wipe what the login established.
func TestIdentityFromIDTokenAndPreservedAcrossRefresh(t *testing.T) {
	isolatedStore(t)
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"sub-1","email":"a@b.c"}`))
	sub, email := identityFromIDToken("header." + payload + ".sig")
	if sub != "sub-1" || email != "a@b.c" {
		t.Errorf("identity = %q/%q", sub, email)
	}
	if sub, _ := identityFromIDToken("garbage"); sub != "" {
		t.Errorf("garbage yielded %q", sub)
	}

	if err := secret.Set(keySub, "sub-1"); err != nil {
		t.Fatal(err)
	}
	if err := secret.Set(keyEmail, "a@b.c"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok_new","expires_in":3600}`))
	}))
	defer server.Close()
	mgr := NewManagerWithOptions(server.URL, &http.Client{})
	if err := storeGrant(tokenGrant{AccessToken: "tok_new", ExpiresIn: 3600}, time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = mgr
	if got, _ := secret.Get(keySub); got != "sub-1" {
		t.Errorf("sub after refresh = %q, want the login value kept", got)
	}
}

// Namespaced, or one variable would wear two vendors' names.
func TestSecretKeysAreNamespaced(t *testing.T) {
	for _, key := range []string{keyAccessToken, keyRefreshToken, keyExpiresAt, keySub, keyEmail} {
		if !strings.HasPrefix(key, "gemini_") {
			t.Errorf("secret key %q is not namespaced", key)
		}
	}
}

// An expired access token with a refresh token alongside is still signed in:
// AccessToken renews it on demand rather than sending the user to the browser.
func TestSignedInCountsARenewableGrant(t *testing.T) {
	isolatedStore(t)
	if SignedIn() {
		t.Fatal("signed in with an empty store")
	}
	if err := secret.Set(keyAccessToken, "tok"); err != nil {
		t.Fatal(err)
	}
	if err := secret.Set(keyRefreshToken, "tok_refresh"); err != nil {
		t.Fatal(err)
	}
	if err := secret.Set(keyExpiresAt, "1"); err != nil {
		t.Fatal(err)
	}
	if !SignedIn() {
		t.Error("an expired access token with a refresh token alongside reads as signed out")
	}
}
