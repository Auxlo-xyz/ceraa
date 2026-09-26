package codexauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ceraa/internal/secret"
)

// fakeOpenAI stands in for auth.openai.com, so the whole flow can be exercised
// without the real service and without a network.
type fakeOpenAI struct {
	mu sync.Mutex

	// approveAfter is how many polls answer "pending" before approval.
	approveAfter int
	polls        int

	// pendingStatus is what a pending poll answers with. OpenAI uses 403.
	pendingStatus int
	// terminalCode, when set, makes the poll fail terminally with that code.
	terminalCode string
	// failUserCode makes minting the code fail.
	failUserCode bool

	// captured request data
	usercodeHits     int
	usercodeRawBody  string
	userCodeClientID string
	pollDeviceID     string
	pollUserCode     string
	exchangeForm     url.Values

	// rotatedRefresh is what the token endpoint hands back, standing in for the
	// rotation OpenAI performs on every refresh.
	rotatedRefresh string
	accessToken    string
}

func (f *fakeOpenAI) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)

	mux.HandleFunc("/usercode", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		fail := f.failUserCode
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"no device auth"}}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			ClientID string `json:"client_id"`
		}
		_ = json.Unmarshal(raw, &payload)
		f.mu.Lock()
		f.usercodeHits++
		f.usercodeRawBody = string(raw)
		f.userCodeClientID = payload.ClientID
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"device_auth_id":"dev-123","user_code":"ABCD-EFGH","expires_at":"` +
			time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339) + `","interval":1}`))
	})

	mux.HandleFunc("/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			DeviceAuthID string `json:"device_auth_id"`
			UserCode     string `json:"user_code"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)

		f.mu.Lock()
		f.polls++
		count := f.polls
		approveAfter := f.approveAfter
		terminal := f.terminalCode
		pendingStatus := f.pendingStatus
		f.pollDeviceID = payload.DeviceAuthID
		f.pollUserCode = payload.UserCode
		f.mu.Unlock()

		if terminal != "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"` + terminal + `","message":"device authorization is not enabled for this account"}}`))
			return
		}
		if count <= approveAfter {
			if pendingStatus == 0 {
				pendingStatus = http.StatusForbidden
			}
			w.WriteHeader(pendingStatus)
			_, _ = w.Write([]byte(`{"error":{"code":"deviceauth_authorization_pending","message":"waiting"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"authorization_code":"auth-code-1","code_verifier":"verifier-1","code_challenge":"challenge-1"}`))
	})

	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.exchangeForm = cloneValues(r.Form)
		access := f.accessToken
		rotated := f.rotatedRefresh
		f.mu.Unlock()
		if access == "" {
			access = "access-1"
		}
		if rotated == "" {
			rotated = "refresh-1"
		}
		_, _ = w.Write([]byte(`{"access_token":"` + access + `","refresh_token":"` + rotated +
			`","expires_in":3600,"token_type":"Bearer"}`))
	})
	t.Cleanup(server.Close)
	return server
}

func cloneValues(values url.Values) url.Values {
	copied := url.Values{}
	for key, list := range values {
		copied[key] = append([]string(nil), list...)
	}
	return copied
}

func (f *fakeOpenAI) setApproveAfter(after int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approveAfter = after
}

func (f *fakeOpenAI) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

func (f *fakeOpenAI) setTerminal(code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminalCode = code
}

func newTestManager(t *testing.T) (*Manager, *fakeOpenAI) {
	t.Helper()
	// An isolated credential store, so a test never reads or writes real tokens.
	t.Setenv("CERAA_HOME", t.TempDir())
	dir := os.Getenv("CERAA_HOME")
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	fake := &fakeOpenAI{}
	server := fake.server(t)
	manager := NewManagerWithOptions(
		Endpoints{UserCode: server.URL + "/usercode", DeviceToken: server.URL + "/deviceauth/token", OAuthToken: server.URL + "/oauth/token"},
		"app_test", server.Client())
	// A tight cadence so the poll loop is exercised without real waits.
	manager.firstPoll = 10 * time.Millisecond
	return manager, fake
}

// TestFullDeviceLoginSucceeds is the flow the whole feature exists for: mint a
// code, wait for approval, exchange it, and end up with a usable token.
func TestFullDeviceLoginSucceeds(t *testing.T) {
	manager, fake := newTestManager(t)
	fake.setApproveAfter(2)

	flow, err := manager.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if state := flow.Snapshot(); state.UserCode != "ABCD-EFGH" {
		t.Fatalf("user code = %q, want it minted immediately", state.UserCode)
	}
	if state := flow.Snapshot(); !strings.Contains(state.AuthURL, "/codex/device") {
		t.Fatalf("verification url = %q", state.AuthURL)
	}

	waitForStatus(t, flow, StatusSucceeded)

	if !Authenticated() {
		t.Fatal("a usable token should be stored after a successful login")
	}
	token, err := secret.Get(keyAccessToken)
	if err != nil || token != "access-1" {
		t.Fatalf("access token = %q, %v", token, err)
	}

	fake.mu.Lock()
	clientID := fake.userCodeClientID
	deviceID, userCode := fake.pollDeviceID, fake.pollUserCode
	form := cloneValues(fake.exchangeForm)
	fake.mu.Unlock()

	if clientID != "app_test" {
		fake.mu.Lock()
		hits, rawBody := fake.usercodeHits, fake.usercodeRawBody
		fake.mu.Unlock()
		t.Fatalf("client id = %q (hits=%d raw=%q), want the configured one; exchange form was %v", clientID, hits, rawBody, form)
	}
	if deviceID != "dev-123" || userCode != "ABCD-EFGH" {
		t.Fatalf("poll sent device=%q user=%q", deviceID, userCode)
	}
	if form.Get("grant_type") != "authorization_code" {
		t.Fatalf("grant type = %q", form.Get("grant_type"))
	}
	if form.Get("code") != "auth-code-1" {
		t.Fatalf("authorization code = %q", form.Get("code"))
	}
	if form.Get("code_verifier") != "verifier-1" {
		t.Fatalf("code verifier = %q", form.Get("code_verifier"))
	}
	if form.Get("redirect_uri") != redirectURI {
		t.Fatalf("redirect uri = %q, want it bound to %q", form.Get("redirect_uri"), redirectURI)
	}
}

// TestPendingPollsAreNotFailures pins that an unanswered poll keeps the flow
// alive. Treating "not yet" as fatal would end every login immediately.
func TestPendingPollsAreNotFailures(t *testing.T) {
	manager, fake := newTestManager(t)
	fake.setApproveAfter(5)

	flow, err := manager.Start()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	if status := flow.Snapshot().Status; status == StatusFailed {
		t.Fatalf("a pending poll must not fail the flow, got %q", flow.Snapshot().Message)
	}
	if fake.pollCount() < 2 {
		t.Fatalf("expected repeated polling, got %d polls", fake.pollCount())
	}
	_ = manager.Cancel(flow.Snapshot().FlowID)
}

// TestPendingOnDifferentStatuses pins that the pending code is honoured on 403
// and on 404, because OpenAI does both.
func TestPendingOnDifferentStatuses(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			manager, fake := newTestManager(t)
			fake.pendingStatus = status
			fake.setApproveAfter(2)

			flow, err := manager.Start()
			if err != nil {
				t.Fatal(err)
			}
			waitForStatus(t, flow, StatusSucceeded)
		})
	}
}

// TestTerminalPollFailsWithTheProvidersReason pins that a real refusal is
// reported with what the provider said, and surfaces the account setting that
// causes it. A generic failure here is what leaves a user stuck.
func TestTerminalPollFailsWithTheProvidersReason(t *testing.T) {
	manager, fake := newTestManager(t)
	fake.setTerminal("deviceauth_disabled")

	flow, err := manager.Start()
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, flow, StatusFailed)

	state := flow.Snapshot()
	if !strings.Contains(state.Message, "not enabled") {
		t.Fatalf("message should carry the provider's reason: %q", state.Message)
	}
	if !strings.Contains(state.Hint, "Enable device code authorization") {
		t.Fatalf("hint should name the setting to change: %q", state.Hint)
	}
	if !strings.Contains(state.Hint, "security settings") {
		t.Fatalf("hint should say where the setting lives: %q", state.Hint)
	}
}

// TestDeviceSettingHintIsShownUpFront pins that the prerequisite is visible
// before the user waits, because when the setting is off the poll never succeeds.
func TestDeviceSettingHintIsShownUpFront(t *testing.T) {
	manager, _ := newTestManager(t)
	flow, err := manager.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Cancel(flow.Snapshot().FlowID)
	message := flow.ResultMessage()
	if !strings.Contains(message, "Enable device code authorization") {
		t.Fatalf("the waiting message should carry the prerequisite: %q", message)
	}
	if !strings.Contains(message, "ABCD-EFGH") || !strings.Contains(message, "/codex/device") {
		t.Fatalf("the waiting message should carry the code and page: %q", message)
	}
}

// TestExpiredFlowFailsRatherThanHanging pins that an abandoned login ends.
func TestExpiredFlowFailsRatherThanHanging(t *testing.T) {
	manager, fake := newTestManager(t)
	fake.setApproveAfter(1_000_000)

	flow, err := manager.Start()
	if err != nil {
		t.Fatal(err)
	}
	// Pull the deadline in, rather than waiting fifteen minutes for it.
	flow.mu.Lock()
	flow.expiresAt = time.Now().Add(-time.Second)
	flow.mu.Unlock()

	waitForStatus(t, flow, StatusFailed)
	if Authenticated() {
		t.Fatal("an expired flow must not leave a token behind")
	}
}

// TestCancelIsIdempotentAndNeverPanics pins the crash: the cancel button stays on
// screen after a flow ends, so a second tap must be a quiet no-op.
func TestCancelIsIdempotentAndNeverPanics(t *testing.T) {
	manager, fake := newTestManager(t)
	fake.setApproveAfter(0)

	flow, err := manager.Start()
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, flow, StatusSucceeded)

	// Cancelling a finished flow, and cancelling twice, must both be safe.
	if err := manager.Cancel(flow.Snapshot().FlowID); err != nil {
		t.Fatalf("cancelling a finished flow should be a no-op, got %v", err)
	}
	if err := manager.Cancel("no-such-flow"); err != nil {
		t.Fatalf("cancelling an unknown flow should be a no-op, got %v", err)
	}
	if err := manager.Cancel(flow.Snapshot().FlowID); err != nil {
		t.Fatalf("a third cancel must not error: %v", err)
	}
	// Check stays strict, because an unknown id there is a real mistake.
	if _, err := manager.Check("no-such-flow"); err == nil {
		t.Fatal("Check should reject an unknown flow")
	}
}

// TestFailedMintIsReported pins that a refusal to mint a code surfaces at once,
// rather than leaving the user with a login that will never work.
func TestFailedMintIsReported(t *testing.T) {
	manager, fake := newTestManager(t)
	fake.failUserCode = true
	if _, err := manager.Start(); err == nil {
		t.Fatal("expected an error when the code cannot be minted")
	} else if !strings.Contains(err.Error(), "no device auth") {
		t.Fatalf("error should carry the provider's reason, got %v", err)
	}
}

// TestAccessTokenRefreshesBeforeExpiry pins that a token close to expiry is
// renewed, and that the renewed one is stored.
func TestAccessTokenRefreshesBeforeExpiry(t *testing.T) {
	manager, fake := newTestManager(t)
	fake.accessToken = "access-1"
	fake.rotatedRefresh = "refresh-1"

	// An expiry inside the refresh margin, so a refresh is required.
	expired := time.Now().Add(time.Minute).Unix()
	if err := secret.Set(keyExpiresAt, fmt.Sprintf("%d", expired)); err != nil {
		t.Fatal(err)
	}
	if err := secret.Set(keyAccessToken, "stale"); err != nil {
		t.Fatal(err)
	}
	if err := secret.Set(keyRefreshToken, "refresh-1"); err != nil {
		t.Fatal(err)
	}

	token, err := manager.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if token != "access-1" {
		t.Fatalf("token = %q, want the refreshed one", token)
	}
	stored, _ := secret.Get(keyRefreshToken)
	if stored != "refresh-1" {
		t.Fatalf("rotated refresh token = %q, want it stored", stored)
	}
}

// TestExpiredTokenWithoutARefreshTokenStillReturnsWhatItHas pins that a token
// which cannot be renewed is handed back rather than withheld. Throwing it away
// would lock the user out on a transient refresh problem.
func TestExpiredTokenWithoutARefreshTokenStillReturnsWhatItHas(t *testing.T) {
	manager, _ := newTestManager(t)
	lapsed := time.Now().Add(time.Minute).Unix()
	if err := secret.Set(keyExpiresAt, fmt.Sprintf("%d", lapsed)); err != nil {
		t.Fatal(err)
	}
	if err := secret.Set(keyAccessToken, "lapsed-but-maybe-valid"); err != nil {
		t.Fatal(err)
	}
	token, err := manager.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if token != "lapsed-but-maybe-valid" {
		t.Fatalf("token = %q, want the existing one rather than nothing", token)
	}
}

// TestRefreshWithoutStoredTokenIsAnError pins that a refresh asked for with
// nothing stored says so, rather than reporting success.
func TestRefreshWithoutStoredTokenIsAnError(t *testing.T) {
	manager, _ := newTestManager(t)
	if _, err := manager.RefreshToken(context.Background()); err == nil {
		t.Fatal("expected an error when no refresh token is stored")
	} else if ErrorCode(err) != "not_authenticated" {
		t.Fatalf("error code = %q", ErrorCode(err))
	}
}

// TestFreshTokenIsNotRefreshed pins that a valid token is used as-is, because
// refreshing on every call would rotate the refresh token for no reason.
func TestFreshTokenIsNotRefreshed(t *testing.T) {
	manager, fake := newTestManager(t)
	fresh := time.Now().Add(time.Hour).Unix()
	if err := secret.Set(keyExpiresAt, fmt.Sprintf("%d", fresh)); err != nil {
		t.Fatal(err)
	}
	if err := secret.Set(keyAccessToken, "fresh-token"); err != nil {
		t.Fatal(err)
	}
	before := fake.pollCount()
	token, err := manager.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "fresh-token" {
		t.Fatalf("token = %q, want the stored one", token)
	}
	if fake.pollCount() != before {
		t.Fatal("a fresh token must not trigger a refresh")
	}
}

// TestAccessTokenWithNothingStored pins that no credentials reads as empty rather
// than as an error, so a caller can branch on it.
func TestAccessTokenWithNothingStored(t *testing.T) {
	manager, _ := newTestManager(t)
	if Authenticated() {
		t.Fatal("nothing should be authenticated in a fresh store")
	}
	token, err := manager.AccessToken(context.Background())
	if err != nil || token != "" {
		t.Fatalf("token = %q, err = %v; want empty and no error", token, err)
	}
}

// TestForgetClearsCredentials pins that signing out leaves nothing behind.
func TestForgetClearsCredentials(t *testing.T) {
	newTestManager(t)
	if err := secret.Set(keyAccessToken, "x"); err != nil {
		t.Fatal(err)
	}
	if err := secret.Set(keyRefreshToken, "y"); err != nil {
		t.Fatal(err)
	}
	if err := Forget(); err != nil {
		t.Fatal(err)
	}
	if Authenticated() {
		t.Fatal("credentials should be gone after Forget")
	}
}

// TestPendingDetection pins that only a pending error is treated as pending, so a
// genuine failure is never mistaken for "not yet".
func TestPendingDetection(t *testing.T) {
	pending := newError(pendingCode, "waiting")
	if !IsPending(pending) {
		t.Fatal("a pending error should be recognised")
	}
	if IsPending(newError("deviceauth_disabled", "off")) {
		t.Fatal("a disabled-account error is not pending")
	}
	if IsPending(context.Canceled) {
		t.Fatal("a cancellation is not pending")
	}
	if ErrorCode(pending) != pendingCode {
		t.Fatalf("ErrorCode = %q", ErrorCode(pending))
	}
}

// TestPollIntervalParsing pins the cadence handling, because a garbled interval
// must not turn into a hot loop or a hung one.
func TestPollIntervalParsing(t *testing.T) {
	cases := map[string]time.Duration{
		"5":    5 * time.Second,
		"0":    defaultPollSeconds * time.Second,
		"":     defaultPollSeconds * time.Second,
		"junk": defaultPollSeconds * time.Second,
		"2.5":  2500 * time.Millisecond,
	}
	for input, want := range cases {
		if got := pollInterval(input); got != want {
			t.Errorf("pollInterval(%q) = %s, want %s", input, got, want)
		}
	}
	if got := pollInterval(3.0); got != 3*time.Second {
		t.Errorf("pollInterval(3.0) = %s", got)
	}
}

// TestReadProviderErrorToleratesJunk pins that an unreadable failure body reads as
// transient, so a flow keeps waiting instead of failing on something it cannot
// interpret.
func TestReadProviderErrorToleratesJunk(t *testing.T) {
	if code, message := readProviderError([]byte("<html>nope</html>"), 500); code != "" || message != "" {
		t.Fatalf("junk should read as empty, got %q/%q", code, message)
	}
	code, message := readProviderError([]byte(`{"error":{"code":"x","message":"y"}}`), 403)
	if code != "x" || message != "y" {
		t.Fatalf("code=%q message=%q", code, message)
	}
	// A flat shape, which some providers use.
	code, _ = readProviderError([]byte(`{"code":"flat","message":"m"}`), 400)
	if code != "flat" {
		t.Fatalf("flat code = %q", code)
	}
}

func TestResultMessageNeverClaimsSuccessWhileWaiting(t *testing.T) {
	waiting := renderStatus(State{Status: StatusWaiting, UserCode: "ABCD-EFGH", AuthURL: verificationURL})
	if strings.Contains(waiting, "successful") {
		t.Fatalf("a waiting flow must not read as a success: %q", waiting)
	}
	failed := renderStatus(State{Status: StatusFailed, Message: "code expired"})
	if !strings.Contains(failed, "code expired") {
		t.Fatalf("a failed flow should say why: %q", failed)
	}
}

func waitForStatus(t *testing.T, flow *Flow, want string) State {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last State
	for time.Now().Before(deadline) {
		last = flow.Snapshot()
		if last.Status == want {
			return last
		}
		if last.Status == StatusFailed && want != StatusFailed {
			t.Fatalf("flow failed while waiting for %q: %s", want, last.Message)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("flow never reached %q; it is %q: %s", want, last.Status, last.Message)
	return last
}
