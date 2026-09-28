package minimaxauth

import (
	"context"
	"encoding/json"
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
	// Pinned to the agent product's own public client, from
	// minimax-code/packages/oauth-core/src/contracts.ts. Two clients came before:
	// OpenClaw's, which put "Sign in with MiniMax to OpenClaw" on the consent
	// page, and MiniMax's *CLI* client, which fixed the name but kept the
	// agent-less coding_plan grant, and the account host answered that one with a
	// one-day free trial instead of the account's Token Plan.
	if OAuthClientID != "mcode-public" {
		t.Errorf("client id = %q", OAuthClientID)
	}
	// agent.default is the agent grant. coding_plan was the CLI's, and asking for
	// it is what produced the trial.
	if Scope != "agent.default" {
		t.Errorf("scope = %q, want the agent grant", Scope)
	}
	// The audience is what the account host mints the token against, and it is
	// sent on all three requests. A login that omits it does not fail, it returns
	// a token for no particular backend.
	if Audience != "agent-backend" {
		t.Errorf("audience = %q", Audience)
	}
	// The portal paths are part of the contract, and the previous client id was
	// reached through a different pair entirely (/api/oauth/*).
	if got := DeviceEndpoints(RegionGlobal); got.DeviceCode != "https://account.minimax.io/oauth2/device/code" || got.Token != "https://account.minimax.io/oauth2/token" {
		t.Errorf("device endpoints = %+v", got)
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
				`","verification_uri_complete":"` + verify + `","expires_in":300,"interval":3}`))
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

// A login that carries only the PKCE challenge fails at the token poll with
// "code_verifier is required", which is the live service's answer and not a guess.
func TestTokenPollCarriesTheVerifier(t *testing.T) {
	initSecret(t)
	var codeChallenge, codeVerifier, grantType, codeField, leakedUserCode string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if strings.HasSuffix(r.URL.Path, "/oauth2/device/code") {
			codeChallenge = r.PostForm.Get("code_challenge")
			// Both codes, because that is what the live service returns for this
			// grant. A stub carrying only user_code models the *CLI* grant and will
			// happily agree with a dialect that polls the wrong one, which is how the
			// wrong dialect shipped: the first stub did exactly this and the login
			// failed in production with "device_code and code_verifier are required".
			_, _ = w.Write([]byte(`{"device_code":"D-3","user_code":"U-3","verification_uri":"https://account.minimax.io/oauth-authorize","verification_uri_complete":"https://account.minimax.io/oauth-authorize?user_code=U-3","expires_in":300,"interval":3}`))
			return
		}
		grantType = r.PostForm.Get("grant_type")
		codeField = r.PostForm.Get("device_code")
		leakedUserCode = r.PostForm.Get("user_code")
		codeVerifier = r.PostForm.Get("code_verifier")
		// The live service names both requirements in one message, so the stub
		// accepts the poll only when both are present and non-empty.
		if codeField == "" || codeVerifier == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"device_code and code_verifier are required"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"acc-m","refresh_token":"ref-m","token_type":"Bearer","expires_in":3600}`))
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
			t.Fatalf("login did not complete, status %q: %s", flow.Snapshot().Status, flow.Snapshot().Message)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if codeChallenge == "" {
		t.Error("the device-code request carried no PKCE challenge")
	}
	if codeVerifier == "" {
		t.Error("the token poll carried no code_verifier; the service answers 'code_verifier is required'")
	}
	if codeVerifier == codeChallenge {
		t.Error("the verifier and the challenge are the same value, so the challenge is not a hash of the verifier")
	}
	if grantType != oauthdevice.GrantTypeDeviceCode {
		t.Errorf("grant = %q, want the RFC 8628 device-code grant", grantType)
	}
	if codeField != "D-3" {
		t.Errorf("device_code = %q, want the code the service issued; polling the user "+
			"code is answered with 'device_code and code_verifier are required'", codeField)
	}
	if leakedUserCode != "" {
		t.Errorf("the poll carried user_code %q alongside device_code; the endpoint wants one", leakedUserCode)
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
			_, _ = w.Write([]byte(`{"user_code":"U-2","verification_uri":"https://account.minimax.io/oauth-authorize","expires_in":300,"interval":3}`))
		default:
			polls++
			if polls < 2 {
				_, _ = w.Write([]byte(`{"status":"pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"acc-m","refresh_token":"ref-m","token_type":"Bearer","expires_in":3600}`))
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
		_, _ = w.Write([]byte(`{"access_token":"acc-2","token_type":"Bearer","expires_in":3600}`))
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
	dialect := Dialect(first)
	if dialect.ExtraCodeParams.Get("code_challenge_method") != "S256" {
		t.Error("the challenge method is not S256")
	}
	if dialect.ExtraCodeParams.Get("code_challenge") != first.Challenge {
		t.Error("the dialect does not carry the challenge")
	}
	// Both halves travel: the challenge opens the flow and the verifier closes it.
	// Carrying only the challenge is answered with "code_verifier is required".
	if dialect.ExtraTokenParams.Get("code_verifier") != first.Verifier {
		t.Error("the dialect does not carry the verifier, so the token poll is rejected")
	}
	if dialect.ExtraTokenParams.Get("code_verifier") == first.Challenge {
		t.Error("the verifier was replaced by the challenge")
	}
}

// The dialect has to name every divergence, or the standard reader misreads the
// response. These are the values observed from the live service for this grant.
func TestDialectNamesEveryDivergence(t *testing.T) {
	d := Dialect(pkcePair{Verifier: "verifier", Challenge: "challenge"})
	// device_code is what the agent grant returns and what the token endpoint
	// insists on. user_code is kept as the fallback because the CLI grant returns
	// no device_code at all, and the two are served from the same paths.
	if d.CodeField != "device_code" {
		t.Errorf("code field = %q, want device_code", d.CodeField)
	}
	if d.FallbackCodeField != "user_code" {
		t.Errorf("fallback code field = %q, want user_code", d.FallbackCodeField)
	}
	// The agent product posts the RFC 8628 device-code grant to /oauth2/token. The
	// user-code grant belonged to the older /oauth/code endpoint.
	if d.GrantType != "urn:ietf:params:oauth:grant-type:device_code" {
		t.Errorf("grant = %q, want the RFC 8628 device-code grant", d.GrantType)
	}
	if d.ExpiryField != "expires_in" {
		t.Errorf("expiry field = %q", d.ExpiryField)
	}
	// expires_in and interval are plain durations on this grant. The two flags
	// below are the CLI grant's, and they are asserted absent rather than left
	// unwritten: MiniMax serves both dialects from the same paths, so a change to
	// OAuthClientID that kept them would read 300 seconds as 300 milliseconds and
	// the year-58709 expiry as a half-second one, which is a login that cannot
	// complete and says nothing about why.
	if d.ExpiryAbsoluteMilliseconds {
		t.Error("expiry read as an absolute timestamp; this grant returns seconds")
	}
	if d.IntervalIsMilliseconds {
		t.Error("interval read as milliseconds; this grant returns seconds")
	}
	// The base_resp envelope belongs to the CLI grant. Left set, a wrapped success
	// is indistinguishable from a wrapped failure.
	if d.ErrorStatusField != "" {
		t.Errorf("error status path = %q; this grant returns standard OAuth errors", d.ErrorStatusField)
	}
	if d.PendingStatusField != "status" || d.PendingStatusValue != "pending" {
		t.Errorf("pending = %q/%q; a 200 pending body would be read as a token", d.PendingStatusField, d.PendingStatusValue)
	}
}

// The audience is part of the grant and has to reach all three requests. It is the
// parameter whose absence does not fail: the host mints a token for no particular
// backend instead of refusing.
func TestAudienceReachesEveryRequest(t *testing.T) {
	d := Dialect(pkcePair{Verifier: "verifier", Challenge: "challenge"})
	if got := d.ExtraCodeParams.Get("audience"); got != Audience {
		t.Errorf("device-code request audience = %q, want %q", got, Audience)
	}
	if got := d.ExtraRefreshParams.Get("audience"); got != Audience {
		t.Errorf("refresh request audience = %q, want %q", got, Audience)
	}
	// The token exchange carries the verifier instead; the audience belongs to the
	// device-code and refresh requests, as in the reference client.
	if got := d.ExtraTokenParams.Get("code_verifier"); got != "verifier" {
		t.Errorf("token request verifier = %q", got)
	}
	if got := d.ExtraCodeParams.Get("code_challenge_method"); got != "S256" {
		t.Errorf("challenge method = %q, want S256", got)
	}
}

// A credential minted under a different grant must not be reused. The token is
// structurally valid, so nothing about it signals the mismatch; reusing it would
// keep the trial the previous grant started.
func TestACredentialFromAnotherGrantIsRefused(t *testing.T) {
	initSecret(t)
	base := Credential{AccessToken: "acc", RefreshToken: "ref", Region: RegionGlobal}
	if err := Save(base); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := Load(RegionGlobal); err != nil {
		t.Fatalf("a credential from this grant was refused: %v", err)
	}

	// Rewrite the stored record as the CLI grant's would have left it.
	var stored record
	value, err := secret.Get(secretName(RegionGlobal))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*record){
		func(r *record) {
			r.ClientID = "659cf4c1-615c-45f6-a5f6-4bf15eb476e5"
			r.Scope = "openid profile coding_plan"
			r.Audience = ""
		},
		func(r *record) { r.Scope = "openid profile coding_plan" },
		func(r *record) { r.Audience = "" },
		func(r *record) { r.ClientID = ""; r.Scope = ""; r.Audience = "" },
	} {
		other := stored
		mutate(&other)
		rewritten, err := json.Marshal(other)
		if err != nil {
			t.Fatal(err)
		}
		if err := secret.Set(secretName(RegionGlobal), string(rewritten)); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(RegionGlobal); err != ErrGrantMismatch {
			t.Errorf("load = %v, want ErrGrantMismatch for %+v", err, other)
		}
		if SignedIn(RegionGlobal) {
			t.Error("a credential from another grant counted as signed in")
		}
	}
}

// stubEndpoints points the device flow at a test server.
func stubEndpoints(srv *httptest.Server) oauthdevice.Endpoints {
	return oauthdevice.Endpoints{
		DeviceCode: srv.URL + "/oauth2/device/code",
		Token:      srv.URL + "/oauth2/token",
	}
}

// The CLI grant returns no device_code, so the reader has to be able to fall back
// to the user code. Both grants come off the same two paths, so a dialect with no
// fallback works against one and fails against the other.
func TestTheUserCodeIsTheFallbackNotTheOnlyCode(t *testing.T) {
	initSecret(t)
	for _, tc := range []struct {
		name    string
		payload string
		want    string
	}{
		{
			"device_code present, as the agent grant returns it",
			`{"device_code":"D-1","user_code":"U-1","verification_uri":"https://account.minimax.io/oauth-authorize","expires_in":300,"interval":3}`,
			"D-1",
		},
		{
			"device_code absent, as the CLI grant returns it",
			`{"user_code":"U-1","verification_uri":"https://account.minimax.io/oauth-authorize","expires_in":300,"interval":3}`,
			"U-1",
		},
	} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/oauth2/device/code") {
				_, _ = w.Write([]byte(tc.payload))
				return
			}
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			// Answer the poll only for the code the reader chose.
			if r.PostForm.Get("device_code") != tc.want && r.PostForm.Get("user_code") != tc.want {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"device_code and code_verifier are required"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"acc","token_type":"Bearer","expires_in":3600}`))
		}))

		login := NewLoginWithEndpoints(RegionGlobal, srv.Client(), stubEndpoints(srv))
		flow, err := login.Start(context.Background())
		if err != nil {
			srv.Close()
			t.Fatalf("%s: start: %v", tc.name, err)
		}
		deadline := time.After(10 * time.Second)
		for flow.Snapshot().Status != oauthdevice.StatusSucceeded {
			select {
			case <-deadline:
				srv.Close()
				t.Fatalf("%s: login did not complete, status %q: %s", tc.name, flow.Snapshot().Status, flow.Snapshot().Message)
			case <-time.After(50 * time.Millisecond):
			}
		}
		srv.Close()
	}
}
