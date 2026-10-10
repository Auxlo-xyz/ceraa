package traeauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/internal/secret"
)

func isolatedStore(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
}

// The exchange contract is exact: RefreshToken in, Data.Token out, and a
// rejection surfaces the vendor code rather than a bare HTTP status.
func TestExchangeTradesPATForToken(t *testing.T) {
	isolatedStore(t)
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cloudide/api/v3/trae/oauth/ExchangeToken" {
			t.Errorf("exchange path = %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"Data":{"Token":"ide-abc","TokenExpireAt":9999999999999}}`))
	}))
	defer srv.Close()
	SetBaseURL(srv.URL)
	defer SetBaseURL("https://api.trae.ai")

	token, expiresAt, err := exchange(context.Background(), srv.URL, "pat-123")
	if err != nil {
		t.Fatal(err)
	}
	if token != "ide-abc" || expiresAt != 9999999999999 {
		t.Errorf("exchange = %q %d", token, expiresAt)
	}
	if gotBody["RefreshToken"] != "pat-123" {
		t.Errorf("exchange body = %v", gotBody)
	}
}

func TestExchangeRejectsBadPAT(t *testing.T) {
	isolatedStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":4001,"message":"invalid token"}`))
	}))
	defer srv.Close()

	if _, _, err := exchange(context.Background(), srv.URL, "bogus"); err == nil || !strings.Contains(err.Error(), "4001") {
		t.Errorf("expected code 4001 error, got %v", err)
	}
}

// SavePAT stores only after a successful exchange, and AccessToken reuses the
// cached token until it nears expiry.
func TestSavePATAndCachedAccessToken(t *testing.T) {
	isolatedStore(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"Data":{"Token":"ide-live","TokenExpireAt":9999999999999}}`))
	}))
	defer srv.Close()
	SetBaseURL(srv.URL)
	defer SetBaseURL("https://api.trae.ai")

	if err := SavePAT(context.Background(), "pat-xyz"); err != nil {
		t.Fatal(err)
	}
	if !Authenticated() {
		t.Error("Authenticated = false after SavePAT")
	}
	got, err := AccessToken(context.Background())
	if err != nil || got != "ide-live" {
		t.Errorf("AccessToken = %q %v", got, err)
	}
	if calls != 1 {
		t.Errorf("exchange calls = %d, want 1 (cached token reused)", calls)
	}
}

func TestRequestHeadersCarryClientIdentity(t *testing.T) {
	h := RequestHeaders("tok")
	if h["Authorization"] != "Cloud-IDE-JWT tok" {
		t.Errorf("Authorization = %q", h["Authorization"])
	}
	if h["X-App-Id"] == "" || h["X-Ide-Version-Code"] == "" || h["X-Ide-Function"] != "chat" {
		t.Errorf("identity headers = %v", h)
	}
}

// The paste-back accepts a redirect URL: the refresh token rides a query
// param or inside the userJwt JSON blob.
func TestExtractRefreshToken(t *testing.T) {
	for raw, want := range map[string]string{
		"http://127.0.0.1:18787/trae/callback?refreshToken=abc123":                            "abc123",
		"https://x.test/cb?refresh_token=zz9":                                                 "zz9",
		"http://127.0.0.1:18787/trae/callback?userJwt=%7B%22RefreshToken%22%3A%22jwt-r%22%7D": "jwt-r",
		"plain-pat-no-url-shape":                                                              "",
		"":                                                                                    "",
	} {
		if got := ExtractRefreshToken(raw); got != want {
			t.Errorf("ExtractRefreshToken(%q) = %q, want %q", raw, got, want)
		}
	}
}

// The verification URI must carry the client id, the PKCE challenge, and the
// paste-back callback, or the approval page cannot complete the login.
func TestBuildVerificationURI(t *testing.T) {
	uri, err := buildVerificationURI("login.trae.ai", "trace-1", "http://127.0.0.1:18787/trae/callback", "challenge-x", "machine-1", "device-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"client_id=" + loginClientID, "code_challenge=challenge-x", "code_challenge_method=S256", "auth_callback_url="} {
		if !strings.Contains(uri, want) {
			t.Errorf("verification URI missing %q: %s", want, uri)
		}
	}
}

// StartLogin resolves the login host through guidance and returns an
// openable URI.
func TestStartLoginUsesGuidanceHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Result":{"LoginHost":"login.test.invalid"}}`))
	}))
	defer srv.Close()
	original := loginGuidanceHosts
	loginGuidanceHosts = []string{srv.URL}
	defer func() { loginGuidanceHosts = original }()

	flow, err := StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(flow.VerificationURI, "login.test.invalid/authorization") {
		t.Errorf("verification URI = %q", flow.VerificationURI)
	}
	if flow.CodeVerifier == "" {
		t.Error("no PKCE verifier minted")
	}
}
