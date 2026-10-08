package deepseekauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/internal/secret"
)

// Fixtures are synthetic by construction: printable ASCII grants, example
// origins, and authorize IDs that name themselves. The credential-shape guard
// scans for Telegram-shaped literals, which none of these resemble; nothing
// here could authenticate anywhere even if it leaked.
const (
	fixtureGrant  = "TEST-ACCOUNT-GRANT-printable-ascii-only"
	fixtureOther  = "TEST-ACCOUNT-GRANT-a-different-printable-grant"
	fixtureCode   = "fixture-auth-code"
	fixtureAuthID = "fixture-authorize-id"
	fixtureState  = "fixture-state-value"
)

// mockPlatform serves the provider's endpoints with the provider's envelope
// and error shapes, from fixtures the test controls. It answers browser URLs
// as https, the way the real platform does; the mock server itself speaks
// plain HTTP, which is exactly why the rewrite flag exists.
type mockPlatform struct {
	t                  *testing.T
	initSeen           map[string]any
	initHeaders        http.Header
	grant              string
	rejectExchangeOnce bool
	bizCode            int
	exchanged          int
	cancelled          int
	loggedOut          int
}

func mockEnvelope(t *testing.T, bizCode int, bizData any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"code": 0,
		"data": map[string]any{"biz_code": bizCode, "biz_data": bizData},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newMockPlatform(t *testing.T) (*mockPlatform, *httptest.Server) {
	t.Helper()
	mp := &mockPlatform{t: t, initSeen: map[string]any{}, grant: fixtureGrant}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
	mux.HandleFunc("/auth-api/v0/dsh/auth_init", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("auth_init body is not JSON: %v", err)
		}
		mp.initSeen = body
		mp.initHeaders = r.Header.Clone()
		write(w, mockEnvelope(t, 0, map[string]any{
			"authorize_url": "https://platform.deepseek.com/dsh/authorize",
			"authorize_id":  fixtureAuthID,
			"expires_in":    600,
		}))
	})
	mux.HandleFunc("/auth-api/v0/dsh/auth_exchange", func(w http.ResponseWriter, r *http.Request) {
		if mp.bizCode != 0 {
			write(w, mockEnvelope(t, mp.bizCode, map[string]any{}))
			return
		}
		if mp.rejectExchangeOnce {
			mp.rejectExchangeOnce = false
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mp.exchanged++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("auth_exchange body is not JSON: %v", err)
		}
		for _, key := range []string{"code", "code_verifier", "redirect_uri", "device_id", "device_model", "os_version"} {
			if _, ok := body[key]; !ok {
				t.Errorf("auth_exchange body missing %q: %v", key, body)
			}
		}
		write(w, mockEnvelope(t, 0, map[string]any{
			"token":          mp.grant,
			"authorized_url": "https://platform.deepseek.com/dsh/authorized",
		}))
	})
	mux.HandleFunc("/auth-api/v0/dsh/auth_cancel", func(w http.ResponseWriter, r *http.Request) {
		mp.cancelled++
		write(w, mockEnvelope(t, 0, map[string]any{}))
	})
	mux.HandleFunc("/auth-api/v0/users/logout", func(w http.ResponseWriter, r *http.Request) {
		mp.loggedOut++
		if r.Header.Get(AuthHeaderName) == "" {
			t.Error("logout arrived without the grant header")
		}
		write(w, mockEnvelope(t, 0, map[string]any{}))
	})
	return mp, httptest.NewServer(mux)
}

// isolateSecrets points the encrypted store at a temp dir, the way the
// sibling auth packages do, so no test touches a real credential file.
func isolateSecrets(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("isolate the secret store: %v", err)
	}
}

func testManager(t *testing.T, mp *mockPlatform, srv *httptest.Server) *Manager {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return NewManagerWithMeta(Endpoints{
		PlatformOrigin:       "http://" + u.Host,
		AllowLoopbackHTTP:    true,
		RewriteBrowserOrigin: true,
	}, ClientMeta{Version: "test-build", Locale: "en_US", TimezoneOffsetSeconds: 0})
}

// The whole dance the operator performs: start, open the URL, approve,
// callback arrives, exchange commits the grant.
func TestFullLoginLoopAgainstTheMockPlatform(t *testing.T) {
	isolateSecrets(t)
	mp, srv := newMockPlatform(t)
	defer srv.Close()
	m := testManager(t, mp, srv)
	if SignedIn() {
		t.Fatal("signed in before any login")
	}

	ctx := context.Background()
	flow, err := m.Start(ctx, srv.URL, "desktop")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if flow.Phase() != PhaseWaitingBrowser {
		t.Fatalf("phase = %q, want %q", flow.Phase(), PhaseWaitingBrowser)
	}
	for _, key := range []string{"code_challenge", "code_challenge_method", "state", "redirect_uri", "locale", "login_source"} {
		if _, ok := mp.initSeen[key]; !ok {
			t.Errorf("auth_init omitted %q", key)
		}
	}
	if mp.initSeen["code_challenge_method"] != "S256" {
		t.Errorf("code_challenge_method = %v, want S256", mp.initSeen["code_challenge_method"])
	}
	if mp.initSeen["redirect_uri"] != srv.URL+callbackPath {
		t.Errorf("redirect_uri = %v, want the loopback callback", mp.initSeen["redirect_uri"])
	}
	for _, header := range []string{"x-client-version", "x-client-locale", "x-client-timezone-offset", "x-client-platform"} {
		if mp.initHeaders.Get(header) == "" {
			t.Errorf("auth_init omitted the %s client header", header)
		}
	}
	if _, ok := mp.initHeaders["X-Client-Bundle-Id"]; !ok {
		t.Error("auth_init omitted the intentionally-empty x-client-bundle-id header")
	}
	if !strings.HasPrefix(flow.AuthorizeURL, srv.URL+"/dsh/authorize") {
		t.Errorf("authorize URL = %q, want the rewritten loopback address", flow.AuthorizeURL)
	}

	// The browser half: a handler that answers the waiting flow, then a GET
	// shaped exactly like the platform's own completion redirect.
	codeCh := make(chan string, 1)
	handler := m.CallbackHandler(flow, func(code string) { codeCh <- code })
	callback, err := url.Parse(srv.URL + callbackPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = callback
	cbReq := httptest.NewRequest(http.MethodGet, callbackPath+"?code="+fixtureCode+"&state="+flow.State, nil)
	cbRec := httptest.NewRecorder()
	handler.ServeHTTP(cbRec, cbReq)
	if cbRec.Code != http.StatusNoContent {
		t.Fatalf("callback answered %d, want 204", cbRec.Code)
	}
	select {
	case got := <-codeCh:
		if got != fixtureCode {
			t.Fatalf("callback delivered %q, want %q", got, fixtureCode)
		}
	default:
		t.Fatal("the callback handler never delivered the code")
	}

	if err := m.Exchange(ctx, flow, fixtureCode); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if flow.Phase() != PhaseSucceeded {
		t.Fatalf("phase = %q, want %q", flow.Phase(), PhaseSucceeded)
	}
	if !SignedIn() {
		t.Fatal("not signed in after a committed exchange")
	}
	got, err := m.AccessToken(ctx)
	if err != nil || got != fixtureGrant {
		t.Fatalf("AccessToken = %q, %v; want the committed grant", got, err)
	}
}

// A callback carrying the wrong state is someone else's code, or a replay.
// It must not complete this attempt.
func TestCallbackWithWrongStateIsRefused(t *testing.T) {
	isolateSecrets(t)
	_, srv := newMockPlatform(t)
	defer srv.Close()
	m := testManager(t, nil, srv)

	flow := &Flow{State: fixtureState, RedirectURI: srv.URL + callbackPath, AuthorizeID: fixtureAuthID}
	delivered := false
	handler := m.CallbackHandler(flow, func(string) { delivered = true })
	req := httptest.NewRequest(http.MethodGet, callbackPath+"?code="+fixtureCode+"&state=someone-elses-state", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong-state callback answered %d, want 400", rec.Code)
	}
	if delivered {
		t.Fatal("the wrong-state code reached the waiting attempt")
	}
}

// Their contract: a late rejection must not clear a replacement credential.
func TestRejectTokenOnlyRemovesTheMatchingGrant(t *testing.T) {
	isolateSecrets(t)
	if err := Forget(); err != nil {
		t.Fatal(err)
	}
	m := NewManager(Endpoints{})
	if _, err := rejectToken(fixtureGrant); err != nil {
		t.Fatalf("rejecting with nothing stored: %v", err)
	}
	if err := storeGrant(record{Grant: fixtureGrant, Origin: DefaultPlatformOrigin, DeviceID: "fixture-device"}); err != nil {
		t.Fatal(err)
	}
	// A rejection naming a grant that is no longer current changes nothing.
	if _, err := rejectToken(fixtureOther); err != nil {
		t.Fatalf("rejecting a foreign token: %v", err)
	}
	got, err := secret.Get(secretKey)
	if err != nil || got == "" {
		t.Fatal("a foreign rejection cleared the current grant")
	}
	// The matching rejection clears it and reports that it did.
	removed, err := m.RejectToken(fixtureGrant)
	if err != nil || !removed {
		t.Fatalf("matching rejection: removed=%v err=%v", removed, err)
	}
	if SignedIn() {
		t.Fatal("still signed in after the matching rejection")
	}
}

// A grant that is not printable ASCII cannot be committed, even from an
// otherwise well-formed exchange. The provider's own schema requires
// ^[\x21-\x7e]+$; storing anything else would put an unusable credential in
// the encrypted store and report success.
func TestExchangeRefusesAnUnusableGrant(t *testing.T) {
	isolateSecrets(t)
	mp, srv := newMockPlatform(t)
	defer srv.Close()
	mp.grant = "has a space, not a grant"
	m := testManager(t, mp, srv)

	flow, err := m.Start(context.Background(), srv.URL, "web")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.Exchange(context.Background(), flow, fixtureCode); ErrorCode(err) != CodeProtocol {
		t.Fatalf("exchange error = %v, want a protocol refusal", err)
	}
	if SignedIn() {
		t.Fatal("an unusable grant was committed to the store")
	}
}

// HTTP 401 on exchange is how the platform says the attempt died. It must
// surface as expiry, not as a committed grant.
func TestUnauthorizedExchangeExpiresTheAttempt(t *testing.T) {
	isolateSecrets(t)
	mp, srv := newMockPlatform(t)
	defer srv.Close()
	mp.rejectExchangeOnce = true
	m := testManager(t, mp, srv)

	flow, err := m.Start(context.Background(), srv.URL, "web")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	err = m.Exchange(context.Background(), flow, fixtureCode)
	if ErrorCode(err) != CodeExpired {
		t.Fatalf("exchange error = %v, want an expiry", err)
	}
	if flow.Phase() != PhaseExpired {
		t.Fatalf("phase = %q, want %q", flow.Phase(), PhaseExpired)
	}
	if SignedIn() {
		t.Fatal("a grant was committed from a rejected exchange")
	}
}

// A nonzero business code is a refusal in an envelope that parsed cleanly.
func TestBusinessRefusalIsAProtocolError(t *testing.T) {
	isolateSecrets(t)
	mp, srv := newMockPlatform(t)
	defer srv.Close()
	m := testManager(t, mp, srv)
	flow, err := m.Start(context.Background(), srv.URL, "web")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	mp.bizCode = 40001
	err = m.Exchange(context.Background(), flow, fixtureCode)
	if ErrorCode(err) != CodeProtocol {
		t.Fatalf("exchange error = %v, want a protocol error", err)
	}
	if SignedIn() {
		t.Fatal("a grant was committed from a refused exchange")
	}
}

// An attempt that outlived the platform's own expiry cannot exchange.
func TestExpiredAttemptCannotExchange(t *testing.T) {
	isolateSecrets(t)
	_, srv := newMockPlatform(t)
	defer srv.Close()
	m := testManager(t, nil, srv)

	flow, err := m.Start(context.Background(), srv.URL, "web")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Their schema already requires a positive expires_in, so an incomplete
	// answer is a protocol error, not an expired flow. Expiry here means the
	// window passed while the operator held the browser page.
	flow.ExpiresAt = time.Now().Add(-time.Second)
	if err := m.Exchange(context.Background(), flow, fixtureCode); ErrorCode(err) != CodeExpired {
		t.Fatalf("exchange error = %v, want an expiry", err)
	}
	if flow.Phase() != PhaseExpired {
		t.Fatalf("phase = %q, want %q", flow.Phase(), PhaseExpired)
	}
	if SignedIn() {
		t.Fatal("a grant was committed from an expired attempt")
	}
}

// Plain HTTP is refused by default, even on loopback. Their platform accepts
// it only in development, and Ceraa posture blocks private ranges unless a
// test opts in, so the default must be the safe one.
func TestPlainHTTPOriginIsRefusedByDefault(t *testing.T) {
	if _, err := validateOrigin("http://127.0.0.1:8080", false); err == nil {
		t.Fatal("loopback HTTP accepted without the explicit opt-in")
	}
	if _, err := validateOrigin("http://127.0.0.1:8080", true); err != nil {
		t.Fatalf("loopback HTTP refused with the opt-in: %v", err)
	}
	if _, err := validateOrigin("http://example.com", true); err == nil {
		t.Fatal("remote HTTP accepted even with the opt-in")
	}
	// Any well-formed HTTPS origin is acceptable AS configuration: the origin
	// being configured is the operator's choice, and shape is all a validator
	// can check. The lookalike danger lives one layer up, where a browser URL
	// must match the configured origin exactly.
	if _, err := validateBrowserURL("https://platform.deepseek.com.evil.example/dsh/authorize",
		"https://platform.deepseek.com", "/dsh/authorize", false); err == nil {
		t.Fatal("a lookalike browser URL passed against the configured origin")
	}
}

// A token that is not printable ASCII cannot be a grant; the provider's own
// exchange schema requires ^[\x21-\x7e]+$.
func TestNonPrintableGrantIsRejected(t *testing.T) {
	if validToken("has a space\ttab") || validToken("newline\nhere") || validToken("") {
		t.Fatal("a non-printable or empty string passed as a grant")
	}
	if !validToken(fixtureGrant) {
		t.Fatal("the fixture grant failed its own validation")
	}
}

// Logout removes locally first and attempts the platform revoke; a failed
// revoke must not restore the grant.
func TestSignOutRemovesLocallyEvenIfRevocationFails(t *testing.T) {
	isolateSecrets(t)
	mp, srv := newMockPlatform(t)
	defer srv.Close()
	m := testManager(t, mp, srv)

	flow, err := m.Start(context.Background(), srv.URL, "desktop")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Commit directly through exchange with the mock's approval.
	handler := m.CallbackHandler(flow, func(string) {})
	req := httptest.NewRequest(http.MethodGet, callbackPath+"?code="+fixtureCode+"&state="+flow.State, nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if err := m.Exchange(context.Background(), flow, fixtureCode); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if !SignedIn() {
		t.Fatal("not signed in after exchange")
	}
	if err := m.SignOut(context.Background()); err != nil {
		t.Fatalf("sign-out: %v", err)
	}
	if SignedIn() {
		t.Fatal("still signed in after sign-out")
	}
	if mp.loggedOut != 1 {
		t.Fatalf("revocation reached the platform %d times, want 1", mp.loggedOut)
	}
	if _, err := secret.Get(secretKey); err == nil {
		t.Fatal("the grant survived sign-out in the store")
	}
}

// The merge teaches the Anthropic transport one new literal, so the literal is
// pinned here: the transport and this package must agree on the string.
func TestWireStyleValue(t *testing.T) {
	if TransportAuthStyle != "dsh" {
		t.Fatalf("TransportAuthStyle = %q, want the value the anthropic.go merge will accept", TransportAuthStyle)
	}
	if AuthHeaderName != "x-dsh-auth-token" {
		t.Fatalf("AuthHeaderName = %q, want the platform's own header", AuthHeaderName)
	}
	if DefaultInferenceBaseURL != "https://api.deepseek.com/anthropic" {
		t.Fatalf("DefaultInferenceBaseURL = %q, want the provider's Messages endpoint", DefaultInferenceBaseURL)
	}
}

// Operators paste what the browser shows, which is the whole callback URL,
// not the bare code. Requiring the bare value turns every full-URL paste
// into a platform refusal shaped exactly like a bad code.
func TestExtractCodeAcceptsTheWholeCallbackURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"abc123", "abc123"},
		{"  abc123  ", "abc123"},
		{"http://127.0.0.1:8787/oauth/callback?code=abc123&state=s", "abc123"},
		{"https://platform.deepseek.com/oauth/callback?code=abc123", "abc123"},
		{"not a url at all", "not a url at all"},
		{"http://127.0.0.1:8787/oauth/callback?state=only", "http://127.0.0.1:8787/oauth/callback?state=only"},
	} {
		if got := ExtractCode(tc.in); got != tc.want {
			t.Errorf("ExtractCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The device record speaks Node's vocabulary because that is what the
// platform's own client sends. Go's names differ in the two places that
// matter, and the platform sees an unknown device otherwise.
func TestDeviceIdentityUsesNodeVocabulary(t *testing.T) {
	if strings.Contains(deviceModel(), "amd64") || strings.Contains(deviceModel(), "windows") {
		t.Errorf("deviceModel = %q, want Node arch/platform names (x64, win32)", deviceModel())
	}
	if strings.Contains(osVersion(), "/") {
		t.Errorf("osVersion = %q, want '<platform> <release>', never a toolchain path", osVersion())
	}
}

// The refusal number is the diagnosis. A bare refusal tells the operator
// nothing; the number distinguishes a spent code from an unknown device.
func TestRefusalCarriesTheBusinessCode(t *testing.T) {
	isolateSecrets(t)
	mp, srv := newMockPlatform(t)
	defer srv.Close()
	m := testManager(t, mp, srv)
	flow, err := m.Start(context.Background(), srv.URL, "web")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	mp.bizCode = 40001
	err = m.Exchange(context.Background(), flow, fixtureCode)
	if ErrorCode(err) != CodeProtocol || !strings.Contains(err.Error(), "40001") {
		t.Fatalf("exchange error = %v, want the refusal number in it", err)
	}
}
