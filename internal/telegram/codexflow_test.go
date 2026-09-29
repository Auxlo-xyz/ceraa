package telegram

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ceraa/internal/codexauth"
	"ceraa/internal/secret"
	"ceraa/internal/telegram/menus"
)

// A stub of the device-authorization endpoints, so the whole flow can be driven
// without a browser and without real credentials.
//
// The two endpoints are distinct and the distinction matters: the device endpoint
// answers with an authorization code once approved, and only then is the OAuth
// endpoint called to exchange it. A stub that returns tokens from the device
// endpoint would pass a test that the real flow could not.
type stubAuth struct {
	server      *httptest.Server
	approved    atomic.Bool
	denied      atomic.Bool
	deviceCalls atomic.Int32
	oauthCalls  atomic.Int32
}

func newStubAuth(t *testing.T) *stubAuth {
	t.Helper()
	stub := &stubAuth{}
	mux := http.NewServeMux()
	mux.HandleFunc("/device/code", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"device_auth_id": "dev-1",
			"user_code":      "ABCD-EFGH",
			"interval":       1,
		})
	})
	mux.HandleFunc("/device/token", func(w http.ResponseWriter, r *http.Request) {
		stub.deviceCalls.Add(1)
		switch {
		case stub.denied.Load():
			// A code the provider will not honour, which the client treats as
			// terminal rather than as "still waiting".
			// The nested envelope, which is what the client reads. A bare
			// {"error":"code"} string does not unmarshal into it, and the client
			// then sees no code and no message and treats the failure as "still
			// waiting", so the test would pass for the wrong reason.
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": "deviceauth_disabled", "message": "device authorization is disabled",
			}})
		case stub.approved.Load():
			json.NewEncoder(w).Encode(map[string]any{
				"authorization_code": "auth-code",
				"code_verifier":      "verifier",
			})
		default:
			// Pending is a 403 carrying the provider's own code.
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": "deviceauth_authorization_pending", "message": "waiting",
			}})
		}
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		stub.oauthCalls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "acc", "refresh_token": "ref",
			"expires_in": 3600, "token_type": "Bearer",
		})
	})
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *stubAuth) endpoints() codexauth.Endpoints {
	return codexauth.Endpoints{
		UserCode:    s.server.URL + "/device/code",
		DeviceToken: s.server.URL + "/device/token",
		OAuthToken:  s.server.URL + "/oauth/token",
	}
}

// isolateState points the bridge state and the secret store at a temp directory.
//
// Both are resolved from paths.DataDir, and defaultState is built at package
// init, so without this a test writes to the live install's bridge-state.json and
// can activate a provider on the running bot. That is not a theoretical hazard:
// an early version of this test tried to set the Codex provider on the real
// install while asserting that activation worked.
func isolateState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret init: %v", err)
	}
	restoreProviders := stubProviders(t)
	t.Cleanup(restoreProviders)
}

// captureCodexNotifications records what the watcher would have sent.
type codexNotification struct {
	chatID int64
	text   string
	hasKB  bool
	// keyboard is the serialised markup, so a test can assert on which buttons
	// were offered rather than only that some markup existed.
	keyboard string
}

func captureCodexNotifications(t *testing.T) (*[]codexNotification, chan struct{}) {
	t.Helper()
	var mu sync.Mutex
	var seen []codexNotification
	signal := make(chan struct{}, 8)
	previous := codexNotify
	codexNotify = func(_ string, chatID int64, text string, kb map[string]any) {
		mu.Lock()
		serialised, _ := json.Marshal(kb)
		seen = append(seen, codexNotification{chatID: chatID, text: text, hasKB: kb != nil, keyboard: string(serialised)})
		mu.Unlock()
		select {
		case signal <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { codexNotify = previous })
	return &seen, signal
}

// awaitNotification waits for the watcher to report something to a chat.
func awaitNotification(t *testing.T, signal chan struct{}) bool {
	t.Helper()
	select {
	case <-signal:
		return true
	case <-time.After(15 * time.Second):
		return false
	}
}

// swapAuthManager points the package-level manager at a stub for one test.
func swapAuthManager(t *testing.T, mgr *codexauth.Manager) func() {
	t.Helper()
	previous := authMgr
	authMgr = mgr
	return func() { authMgr = previous }
}

// The defect: completion was detected only in the codex:check callback, so a user
// who approved the code and then typed their task kept the old provider with no
// indication a credential had arrived.
func TestCompletionIsReportedWithoutTheCheckTap(t *testing.T) {
	isolateState(t)
	seen, signal := captureCodexNotifications(t)
	stub := newStubAuth(t)
	restore := swapAuthManager(t, codexauth.NewManagerWithOptions(stub.endpoints(), "client", stub.server.Client()))
	defer restore()
	registerCodexCompletion("test-token")

	flow, err := authMgr.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	flowID := flow.Snapshot().FlowID
	const chatID = int64(4242)
	trackCodexFlow(flowID, chatID, true, "")
	defer clearCodexFlow(flowID)
	if got := flow.Snapshot().Status; got == codexauth.StatusSucceeded {
		t.Fatal("flow succeeded before approval; the test would prove nothing")
	}

	stub.approved.Store(true)
	// Nobody taps Check. The approval alone has to produce a message.
	if !awaitNotification(t, signal) {
		t.Fatal("the login completed but the chat was never told")
	}
	if stub.deviceCalls.Load() == 0 {
		t.Fatal("the device endpoint was never polled, so nothing was proven")
	}

	mu := sync.Mutex{}
	mu.Lock()
	notes := append([]codexNotification(nil), (*seen)...)
	mu.Unlock()
	if len(notes) == 0 {
		t.Fatal("no notification recorded")
	}
	if notes[0].chatID != chatID {
		t.Errorf("notified chat = %d, want %d", notes[0].chatID, chatID)
	}
	if !strings.Contains(strings.ToLower(notes[0].text), "codex") {
		t.Errorf("confirmation does not mention Codex: %q", notes[0].text)
	}
	// The flow must be forgotten, or a repeated callback would repost it.
	if _, tracked := codexFlowOwnerOf(flowID); tracked {
		t.Error("a reported flow is still tracked")
	}
}

// A poller cannot work here: a finished flow leaves the registry, so Check never
// returns it. Asserted so the shape of this bug cannot come back.
func TestFinishedFlowIsNotInTheRegistry(t *testing.T) {
	isolateState(t)
	captureCodexNotifications(t)
	stub := newStubAuth(t)
	restore := swapAuthManager(t, codexauth.NewManagerWithOptions(stub.endpoints(), "client", stub.server.Client()))
	defer restore()

	flow, err := authMgr.Start()
	if err != nil {
		t.Fatal(err)
	}
	flowID := flow.Snapshot().FlowID
	stub.approved.Store(true)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if flow.Snapshot().Status == codexauth.StatusSucceeded {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if flow.Snapshot().Status != codexauth.StatusSucceeded {
		t.Fatalf("flow did not succeed: %q %q", flow.Snapshot().Status, flow.Snapshot().Message)
	}
	if _, err := authMgr.Check(flowID); err == nil {
		t.Error("a finished flow is still in the registry, so a poller could find it; the callback is not the only route")
	}
}

// A denied login must be reported with a way to retry, not cleared and forgotten.
func TestFailureIsReportedWithRecovery(t *testing.T) {
	isolateState(t)
	seen, signal := captureCodexNotifications(t)
	stub := newStubAuth(t)
	restore := swapAuthManager(t, codexauth.NewManagerWithOptions(stub.endpoints(), "client", stub.server.Client()))
	defer restore()
	registerCodexCompletion("test-token")

	flow, err := authMgr.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	flowID := flow.Snapshot().FlowID
	trackCodexFlow(flowID, 99, true, "")
	// Denied is set only now, once the flow is tracked, because that is the order
	// a real denial happens in: the code is not shown to anyone until the entry
	// point has registered an owner and posted it, so nobody can deny it before
	// that.
	//
	// Setting it before Start made the first poll terminal, and watch polls before
	// it sleeps, so the completion raced the registration and arrived to find no
	// owner. A local httptest server answers in microseconds, so it won that race
	// often enough to fail the suite. The real endpoint is milliseconds of network
	// against a microsecond map write and does not behave this way, so the ordering
	// was modelling a race nobody gets, not the one under test.
	stub.denied.Store(true)

	if !awaitNotification(t, signal) {
		t.Fatal("a denied login was never reported")
	}
	mu := sync.Mutex{}
	mu.Lock()
	notes := append([]codexNotification(nil), (*seen)...)
	mu.Unlock()
	if len(notes) == 0 {
		t.Fatal("no notification recorded for the failure")
	}
	note := notes[0]
	if note.chatID != 99 {
		t.Errorf("notified chat = %d, want 99", note.chatID)
	}
	if !note.hasKB {
		t.Fatal("the failure report carries no keyboard, so there is no way to retry")
	}
	// A keyboard that exists but offers nothing actionable is the same dead end as
	// no keyboard, which is what the emptied one used to be.
	if !strings.Contains(note.keyboard, "codex:login") {
		t.Errorf("the failure keyboard offers no retry: %s", note.keyboard)
	}
	if _, tracked := codexFlowOwnerOf(flowID); tracked {
		t.Error("a reported failure is still tracked")
	}
}

// A cancelled login is deliberate, so it is not announced as a failure.
func TestCancelIsNotReportedAsAFailure(t *testing.T) {
	isolateState(t)
	seen, signal := captureCodexNotifications(t)
	stub := newStubAuth(t)
	restore := swapAuthManager(t, codexauth.NewManagerWithOptions(stub.endpoints(), "client", stub.server.Client()))
	defer restore()
	registerCodexCompletion("test-token")

	flow, err := authMgr.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	flowID := flow.Snapshot().FlowID
	trackCodexFlow(flowID, 55, true, "")
	authMgr.Cancel(flowID)

	select {
	case <-signal:
		t.Fatalf("a deliberate cancel was announced: %+v", *seen)
	case <-time.After(1500 * time.Millisecond):
	}
	if _, tracked := codexFlowOwnerOf(flowID); tracked {
		t.Error("a cancelled flow is still tracked")
	}
}

// Every terminal state needs a way forward. The keyboard used to be emptied on
// failure and on cancel, leaving a dead message.
func TestEveryTerminalStateOffersAWayForward(t *testing.T) {
	for _, status := range []string{codexauth.StatusSucceeded, codexauth.StatusFailed, codexauth.StatusCanceled} {
		raw, err := codexauth.KeyboardJSONForState(codexauth.State{
			FlowID: "f1", Status: status, AuthURL: "https://example.invalid/approve",
		})
		if err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		var markup struct {
			InlineKeyboard [][]struct {
				Text string `json:"text"`
				URL  string `json:"url"`
				Data string `json:"callback_data"`
			} `json:"inline_keyboard"`
		}
		if err := json.Unmarshal(raw, &markup); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if len(markup.InlineKeyboard) == 0 {
			t.Errorf("%s: keyboard is empty, so the message is a dead end", status)
			continue
		}
		hasAction := false
		for _, row := range markup.InlineKeyboard {
			for _, button := range row {
				if button.Data != "" || button.URL != "" {
					hasAction = true
				}
			}
		}
		if !hasAction {
			t.Errorf("%s: keyboard has no actionable button", status)
		}
	}
}

// An in-flight login keeps the open/check/cancel row, because that is the state
// with something to do.
func TestInFlightLoginKeepsItsActions(t *testing.T) {
	raw, err := codexauth.KeyboardJSONForState(codexauth.State{
		FlowID: "f1", Status: codexauth.StatusWaiting, AuthURL: "https://example.invalid/approve",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Open Authorization Page", "codex:check:", "codex:cancel:"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("waiting keyboard is missing %q: %s", want, raw)
		}
	}
}

// A login started from the typed command must be tracked too. It used to be
// invisible to the completion path, so it could never activate anything.

// The hub must name Codex, or status stays a command nobody discovers.
func TestHubNamesCodex(t *testing.T) {
	hub := menus.Hub()
	raw, _ := json.Marshal(hub)
	if !strings.Contains(string(raw), "codex") {
		t.Errorf("hub does not offer a Codex entry: %s", raw)
	}
	if !strings.Contains(string(raw), "Codex") {
		t.Errorf("hub has no human-readable Codex label: %s", raw)
	}
}

// The status keyboard must never tell the user to type, and must offer the
// action its state calls for.
func TestCodexStatusKeyboardMatchesTheState(t *testing.T) {
	cases := []struct {
		name          string
		authenticated bool
		active        bool
		want          string
	}{
		{"not signed in", false, false, "codex:login"},
		{"signed in, other provider", true, false, "codex:activate"},
		{"signed in and active", true, true, "codex:login"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(codexStatusKeyboard(tc.authenticated, tc.active))
			if !strings.Contains(string(raw), tc.want) {
				t.Errorf("keyboard %s does not offer %s", raw, tc.want)
			}
			if strings.Contains(string(raw), "/codex") {
				t.Errorf("keyboard %s tells the user to type a command", raw)
			}
		})
	}
}
