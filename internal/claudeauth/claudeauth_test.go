package claudeauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/internal/secret"
)

// isolateSecrets points the encrypted store at a temp dir, the way the
// sibling auth packages do, so no test touches a real credential file.
func isolateSecrets(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("isolate the secret store: %v", err)
	}
}

// mockTokenServer answers token requests and records what arrived.
type mockTokenServer struct {
	t            *testing.T
	exchangeSeen url.Values
	refreshSeen  url.Values
	userAgent    string
	exchangeBody func() map[string]any
	refreshBody  func() (map[string]any, int)
}

func newMockTokenServer(t *testing.T) (*mockTokenServer, *httptest.Server) {
	t.Helper()
	mp := &mockTokenServer{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		mp.userAgent = r.Header.Get("User-Agent")
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		form, err := url.ParseQuery(string(raw))
		if err != nil || form.Get("grant_type") == "" {
			// JSON exchange body: flatten string fields into form.
			var decoded map[string]any
			if jerr := json.Unmarshal(raw, &decoded); jerr != nil {
				t.Errorf("token request is neither form nor JSON: %q", raw)
			}
			form = url.Values{}
			for k, v := range decoded {
				if s, ok := v.(string); ok {
					form.Set(k, s)
				}
			}
		}
		var answer map[string]any
		status := http.StatusOK
		switch form.Get("grant_type") {
		case "authorization_code":
			mp.exchangeSeen = form
			answer = mp.exchangeBody()
		case "refresh_token":
			mp.refreshSeen = form
			answer, status = mp.refreshBody()
		default:
			t.Errorf("unknown grant_type %q", form.Get("grant_type"))
			answer = map[string]any{"error": "unsupported_grant_type"}
			status = http.StatusBadRequest
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(answer)
	})
	return mp, httptest.NewServer(mux)
}

func testManager(mp *mockTokenServer, srv *httptest.Server) *Manager {
	u, err := url.Parse(srv.URL)
	if err != nil {
		mp.t.Fatal(err)
	}
	return NewManager(Endpoints{
		TokenURLs:         []string{"http://" + u.Host + "/v1/oauth/token"},
		AllowLoopbackHTTP: true,
	})
}

// Start builds the browser URL locally: no network, PKCE S256, state.
func TestStartBuildsAuthorizeURLWithoutNetwork(t *testing.T) {
	m := NewManager(Endpoints{})
	flow, err := m.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if flow.Phase() != PhaseWaitingCode {
		t.Fatalf("phase = %q, want %q", flow.Phase(), PhaseWaitingCode)
	}
	parsed, err := url.Parse(flow.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize URL is not a URL: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "claude.ai" || parsed.Path != "/oauth/authorize" {
		t.Errorf("authorize URL host/path = %s%s, want claude.ai/oauth/authorize", parsed.Host, parsed.Path)
	}
	query := parsed.Query()
	if query.Get("client_id") != OAuthClientID {
		t.Errorf("client_id = %q, want the shared client", query.Get("client_id"))
	}
	if query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" {
		t.Error("PKCE S256 challenge missing")
	}
	if query.Get("state") != flow.State || flow.State == "" {
		t.Error("state missing or not the flow's own")
	}
	if query.Get("redirect_uri") != RedirectURI {
		t.Errorf("redirect_uri = %q, want the fixed callback", query.Get("redirect_uri"))
	}
	if !strings.Contains(query.Get("scope"), "user:inference") {
		t.Errorf("scope = %q, must include user:inference", query.Get("scope"))
	}
	if flow.Verifier == "" {
		t.Error("verifier missing: nothing to exchange with")
	}
}

// The whole operator dance: authorize URL, paste the code, exchange commits.
func TestFullLoginLoopAgainstMockTokenServer(t *testing.T) {
	isolateSecrets(t)
	mp, srv := newMockTokenServer(t)
	defer srv.Close()
	mp.exchangeBody = func() map[string]any {
		return map[string]any{"access_token": "sk-ant-oat-test", "refresh_token": "rt-test", "expires_in": 3600}
	}
	mp.refreshBody = func() (map[string]any, int) {
		return map[string]any{"access_token": "sk-ant-oat-new"}, http.StatusOK
	}
	m := testManager(mp, srv)
	if SignedIn() {
		t.Fatal("signed in before any login")
	}
	flow, err := m.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.Exchange(context.Background(), flow, "spl-op-testcode"); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if flow.Phase() != PhaseSucceeded {
		t.Fatalf("phase = %q, want succeeded", flow.Phase())
	}
	if mp.exchangeSeen.Get("code") != "spl-op-testcode" {
		t.Errorf("exchange sent code = %q", mp.exchangeSeen.Get("code"))
	}
	if mp.exchangeSeen.Get("code_verifier") != flow.Verifier {
		t.Error("exchange did not send the flow's own verifier")
	}
	if mp.exchangeSeen.Get("client_id") != OAuthClientID {
		t.Errorf("exchange client_id = %q", mp.exchangeSeen.Get("client_id"))
	}
	if mp.userAgent != TokenEndpointUserAgent {
		t.Errorf("token UA = %q, want %q", mp.userAgent, TokenEndpointUserAgent)
	}
	if !SignedIn() {
		t.Fatal("not signed in after a committed exchange")
	}
	got, err := m.AccessToken(context.Background())
	if err != nil || got != "sk-ant-oat-test" {
		t.Fatalf("access token = %q, %v", got, err)
	}
}

// A code carrying another attempt's state is refused before the network.
func TestExchangeRejectsForeignState(t *testing.T) {
	isolateSecrets(t)
	mp, srv := newMockTokenServer(t)
	defer srv.Close()
	mp.exchangeBody = func() map[string]any {
		return map[string]any{"access_token": "sk-ant-oat-test"}
	}
	m := testManager(mp, srv)
	flow, err := m.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.Exchange(context.Background(), flow, "somecode#not-our-state"); ErrorCode(err) != CodeProtocol {
		t.Fatalf("foreign state exchanged or wrong code: %v", err)
	}
	if mp.exchangeSeen != nil {
		t.Error("the refused code still reached the token endpoint")
	}
}

// Past the expiry buffer, AccessToken refreshes form-encoded and stores.
func TestAccessTokenRefreshesPastTheBuffer(t *testing.T) {
	isolateSecrets(t)
	mp, srv := newMockTokenServer(t)
	defer srv.Close()
	mp.exchangeBody = func() map[string]any {
		return map[string]any{"access_token": "sk-ant-oat-old", "refresh_token": "rt-old", "expires_in": 3600}
	}
	mp.refreshBody = func() (map[string]any, int) {
		return map[string]any{"access_token": "sk-ant-oat-new", "refresh_token": "rt-new", "expires_in": 3600}, http.StatusOK
	}
	m := testManager(mp, srv)
	flow, err := m.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.Exchange(context.Background(), flow, "code1"); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	// Two hours later: the stored hour-long grant is past the buffer.
	later := NewManager(Endpoints{
		TokenURLs:         m.endpoints.TokenURLs,
		AllowLoopbackHTTP: true,
		Now:               func() time.Time { return time.Now().Add(2 * time.Hour) },
	})
	got, err := later.AccessToken(context.Background())
	if err != nil || got != "sk-ant-oat-new" {
		t.Fatalf("refreshed token = %q, %v", got, err)
	}
	if mp.refreshSeen.Get("grant_type") != "refresh_token" || mp.refreshSeen.Get("refresh_token") != "rt-old" {
		t.Errorf("refresh sent %v", mp.refreshSeen)
	}
	if mp.refreshSeen.Get("client_id") != OAuthClientID {
		t.Errorf("refresh client_id = %q", mp.refreshSeen.Get("client_id"))
	}
	// And the rotation committed: the next call serves it without the network.
	mp.refreshSeen = nil
	got, err = later.AccessToken(context.Background())
	if err != nil || got != "sk-ant-oat-new" {
		t.Fatalf("committed rotation not served: %q, %v", got, err)
	}
	if mp.refreshSeen != nil {
		t.Error("a committed grant still refreshed")
	}
}

// A terminal grant error means relogin, never a replay.
func TestTerminalGrantMeansRelogin(t *testing.T) {
	isolateSecrets(t)
	mp, srv := newMockTokenServer(t)
	defer srv.Close()
	mp.exchangeBody = func() map[string]any {
		return map[string]any{"access_token": "sk-ant-oat-old", "refresh_token": "rt-dead", "expires_in": 3600}
	}
	mp.refreshBody = func() (map[string]any, int) {
		return map[string]any{"error": "invalid_grant", "error_description": "revoked"}, http.StatusBadRequest
	}
	m := testManager(mp, srv)
	flow, err := m.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.Exchange(context.Background(), flow, "code1"); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	later := NewManager(Endpoints{
		TokenURLs:         m.endpoints.TokenURLs,
		AllowLoopbackHTTP: true,
		Now:               func() time.Time { return time.Now().Add(2 * time.Hour) },
	})
	if _, err := later.AccessToken(context.Background()); !ReloginRequired(err) {
		t.Fatalf("dead grant error = %v, want grant-dead", err)
	}
}

// RejectToken only clears while the stored grant still matches.
func TestRejectTokenMatchesBeforeClearing(t *testing.T) {
	isolateSecrets(t)
	mp, srv := newMockTokenServer(t)
	defer srv.Close()
	mp.exchangeBody = func() map[string]any {
		return map[string]any{"access_token": "sk-ant-oat-live", "refresh_token": "rt-live", "expires_in": 3600}
	}
	m := testManager(mp, srv)
	flow, _ := m.Start()
	if err := m.Exchange(context.Background(), flow, "code1"); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if cleared, _ := m.RejectToken("sk-ant-oat-stale"); cleared {
		t.Fatal("a stale token cleared the live grant")
	}
	if !SignedIn() {
		t.Fatal("the live grant is gone after a mismatched rejection")
	}
	if cleared, _ := m.RejectToken("sk-ant-oat-live"); !cleared {
		t.Fatal("the matching grant was not cleared")
	}
	if SignedIn() {
		t.Fatal("still signed in after clearing the grant")
	}
}

func TestExtractCodeShapes(t *testing.T) {
	if got := ExtractCode("spl-op-abc123"); got.code != "spl-op-abc123" || got.state != "" {
		t.Errorf("bare code misread: %+v", got)
	}
	if got := ExtractCode("spl-op-abc123#mystate"); got.code != "spl-op-abc123" || got.state != "mystate" {
		t.Errorf("code#state misread: %+v", got)
	}
	if got := ExtractCode("https://console.anthropic.com/oauth/code/callback?code=spl-op-x&state=s"); got.code != "spl-op-x" {
		t.Errorf("callback URL misread: %+v", got)
	}
}

// The merge teaches the Anthropic transport one new literal, so the literal
// is pinned here: the transport and this package must agree on the string.
func TestWireStyleValue(t *testing.T) {
	if TransportAuthStyle != "claude-oauth" {
		t.Fatalf("TransportAuthStyle = %q, want the value the anthropic.go merge will accept", TransportAuthStyle)
	}
	if DefaultInferenceBaseURL != "https://api.anthropic.com" {
		t.Fatalf("DefaultInferenceBaseURL = %q, want the native Messages endpoint", DefaultInferenceBaseURL)
	}
	for _, beta := range OAuthBetaHeaders {
		if beta != "claude-code-20250219" && beta != "oauth-2025-04-20" {
			t.Fatalf("unexpected OAuth beta %q: the pair is the provider's own", beta)
		}
	}
}

func TestCancelOnlyWithdrawsWaitingAttempts(t *testing.T) {
	m := NewManager(Endpoints{})
	flow, err := m.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.Cancel(flow); err != nil {
		t.Fatalf("cancel a waiting attempt: %v", err)
	}
	if flow.Phase() != PhaseCancelled {
		t.Fatalf("phase = %q, want cancelled", flow.Phase())
	}
	if err := m.Cancel(flow); ErrorCode(err) != CodeProtocol {
		t.Fatalf("second cancel = %v, want a protocol refusal", err)
	}
}
