package minimaxauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

// The constants decide whether sign-in works at all, and a typo in any of them
// fails at the provider rather than here.
func TestConstantsMatchTheService(t *testing.T) {
	if OAuthClientID != "78257093-7e40-4613-99e0-527b14b39113" {
		t.Errorf("client id = %q", OAuthClientID)
	}
	// model.completion is what makes the token able to invoke a model. A login
	// without it authenticates and then fails on every request.
	for _, want := range []string{"model.completion", "group_id", "profile"} {
		if !strings.Contains(Scope, want) {
			t.Errorf("scope lost %q", want)
		}
	}
	hosts := endpointsFor(RegionGlobal)
	if hosts.OAuthBase != "https://account.minimax.io" {
		t.Errorf("global oauth host = %q", hosts.OAuthBase)
	}
	// The subscription is served by the Messages API, so the base carries the
	// /anthropic suffix. The plain host is the API-key provider's.
	if !strings.HasSuffix(hosts.Base, "/anthropic") {
		t.Errorf("global inference base = %q, want the /anthropic path", hosts.Base)
	}
	cn := endpointsFor(RegionCN)
	if cn.OAuthBase != "https://account.minimaxi.com" {
		t.Errorf("cn oauth host = %q", cn.OAuthBase)
	}
	if cn.OAuthBase == hosts.OAuthBase {
		t.Error("the two regions share an oauth host, so they cannot be separate accounts")
	}
}

// The verification URL is the only thing the user can act on. Dropping it produces
// a code with nowhere to go, which a stub that happens to include the field will
// not reveal.
func TestVerificationURLSurvivesTheLogin(t *testing.T) {
	initSecret(t)
	const verify = "https://platform.minimax.io/oauth-authorize?user_code=U-1"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/oauth2/device/code") {
			_, _ = w.Write([]byte(`{"user_code":"U-1","verification_uri":"` + verify +
				`","verification_uri_complete":"` + verify + `&client=x","expired_in":1790519361298,"interval":3000}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"pending"}`))
	}))
	defer srv.Close()

	login := NewLoginWithEndpoints(RegionGlobal, srv.Client(), stubEndpoints(srv))
	flow, err := login.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer login.Manager().Cancel(flow.ID)

	got := flow.Snapshot().VerificationURL
	if got == "" {
		t.Fatal("the verification URL was dropped; the user has a code with nowhere to enter it")
	}
	if !strings.Contains(got, "user_code=U-1") {
		t.Errorf("verification URL = %q", got)
	}
}

// A completed login has to persist, or it reports success and leaves nothing.
func TestCompletedLoginIsPersisted(t *testing.T) {
	initSecret(t)
	var polls int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/device/code"):
			_, _ = w.Write([]byte(`{"user_code":"U-2","verification_uri":"https://platform.minimax.io/x","expired_in":1790519361298,"interval":300}`))
		default:
			polls++
			if polls < 2 {
				_, _ = w.Write([]byte(`{"status":"pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"acc-m","refresh_token":"ref-m","token_type":"Bearer","expired_in":1790519361298}`))
		}
	}))
	defer srv.Close()

	login := NewLoginWithEndpoints(RegionGlobal, srv.Client(), stubEndpoints(srv))
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
	if !SignedIn(RegionGlobal) {
		t.Fatal("the login succeeded but nothing was stored")
	}
	credential, err := Load(RegionGlobal)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if credential.AccessToken != "acc-m" {
		t.Errorf("access token = %q", credential.AccessToken)
	}
	if !strings.HasSuffix(credential.BaseURL, "/anthropic") {
		t.Errorf("base URL = %q, want the messages endpoint", credential.BaseURL)
	}
	// The absolute epoch-ms expiry must resolve to a real time, not the far future.
	if y := credential.ExpiresAt.Year(); y < 2020 || y > 2100 {
		t.Errorf("expiry year is %d; the absolute timestamp was read as a duration", y)
	}
}

// The regions are separate accounts, so a global credential must not satisfy a CN
// lookup or the user signs in to the wrong deployment.
func TestRegionsAreStoredSeparately(t *testing.T) {
	initSecret(t)
	if err := Save(Credential{AccessToken: "global-token", Region: RegionGlobal}); err != nil {
		t.Fatal(err)
	}
	if !SignedIn(RegionGlobal) {
		t.Error("the global credential is not visible in its own region")
	}
	if SignedIn(RegionCN) {
		t.Error("a global credential was found under the cn region")
	}
}

func TestSignedInRejectsATokenlessRecord(t *testing.T) {
	initSecret(t)
	if SignedIn(RegionGlobal) {
		t.Error("SignedIn with nothing stored")
	}
	if err := Save(Credential{RefreshToken: "ref-1", Region: RegionGlobal}); err == nil {
		t.Error("a credential with no access token was stored; it would read as signed in and fail on first use")
	}
}

// A refresh response usually omits refresh_token, meaning keep the one you have.
func TestRefreshKeepsTheExistingRefreshToken(t *testing.T) {
	initSecret(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"acc-2","token_type":"Bearer","expired_in":1790519361298}`))
	}))
	defer srv.Close()

	if err := Save(Credential{AccessToken: "old", RefreshToken: "ref-1", Region: RegionGlobal, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	store := NewStore(RegionGlobal)
	store.HTTPClient = srv.Client()
	store.Endpoints = stubEndpoints(srv)
	credential, err := Resolve(context.Background(), store)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if credential.AccessToken != "acc-2" {
		t.Errorf("access token = %q", credential.AccessToken)
	}
	if credential.RefreshToken != "ref-1" {
		t.Errorf("refresh token = %q; it must be kept when the response omits it", credential.RefreshToken)
	}
}

func TestExpiredWithoutRefreshTokenIsReported(t *testing.T) {
	initSecret(t)
	if err := Save(Credential{AccessToken: "a", Region: RegionGlobal, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(context.Background(), NewStore(RegionGlobal)); err == nil {
		t.Error("an expired credential with no refresh token resolved silently")
	}
}

// The PKCE challenge must be per-login. A fixed one would be a replayable secret
// sitting in the source.
func TestPKCEChallengeIsUniquePerLogin(t *testing.T) {
	first, err := newPKCE()
	if err != nil {
		t.Fatal(err)
	}
	second, err := newPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if first.Challenge == second.Challenge {
		t.Error("two logins produced the same challenge")
	}
	if first.Verifier == "" || first.Challenge == first.Verifier {
		t.Error("the challenge is not a hash of the verifier")
	}
	dialect := Dialect(first.Challenge)
	if dialect.ExtraCodeParams.Get("code_challenge_method") != "S256" {
		t.Error("the challenge method is not S256")
	}
	if dialect.ExtraCodeParams.Get("code_challenge") != first.Challenge {
		t.Error("the dialect does not carry the challenge")
	}
}

// The dialect has to name every divergence, or the standard reader misreads the
// response. These are the values observed from the live service.
func TestDialectNamesEveryDivergence(t *testing.T) {
	d := Dialect("challenge")
	if d.CodeField != "user_code" {
		t.Errorf("code field = %q", d.CodeField)
	}
	if d.GrantType != "urn:ietf:params:oauth:grant-type:user_code" {
		t.Errorf("grant = %q", d.GrantType)
	}
	if d.ExpiryField != "expired_in" {
		t.Errorf("expiry field = %q", d.ExpiryField)
	}
	if !d.ExpiryAbsoluteMilliseconds {
		t.Error("the expiry is absolute, not a duration")
	}
	if !d.IntervalIsMilliseconds {
		t.Error("the interval is in milliseconds")
	}
	if d.PendingStatusField != "status" || d.PendingStatusValue != "pending" {
		t.Errorf("pending = %q/%q; a 200 pending body would be read as a token", d.PendingStatusField, d.PendingStatusValue)
	}
	if d.ErrorStatusField != "base_resp.status_code" {
		t.Errorf("error status path = %q; a flat lookup finds nothing", d.ErrorStatusField)
	}
}

// stubEndpoints points the device flow at a test server.
func stubEndpoints(srv *httptest.Server) oauthdevice.Endpoints {
	return oauthdevice.Endpoints{
		DeviceCode: srv.URL + "/oauth2/device/code",
		Token:      srv.URL + "/oauth2/token",
	}
}
