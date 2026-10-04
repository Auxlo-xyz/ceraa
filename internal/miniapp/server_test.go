package miniapp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"ceraa/harness/event"
	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
	"ceraa/internal/secret"
)

const (
	testToken = "123456789:AAFixtureTokenForMiniAppTests"
	testUser  = int64(279058397)
)

// initDataFor signs a payload the way a Telegram client would, so the tests
// below exercise verification rather than a stub that always says yes.
func initDataFor(t *testing.T, token string, userID int64, age time.Duration) string {
	t.Helper()
	values := url.Values{}
	values.Set("auth_date", fmt.Sprintf("%d", time.Now().Add(-age).Unix()))
	values.Set("user", fmt.Sprintf(`{"id":%d,"first_name":"V","username":"v"}`, userID))
	values.Set("hash", hashOf(token, dataCheckString(values)))
	return values.Encode()
}

// newFixture builds a server over a store that already holds one session.
func newFixture(t *testing.T, allowID int64) (*Server, *sessionstore.Store, *event.Bus) {
	t.Helper()
	store, err := sessionstore.New(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	sess := &sessionstore.Session{
		ID:      "ses-fixture",
		Title:   "Fixture session",
		Version: 1,
		Messages: []sessionstore.Message{
			{Role: llm.RoleUser, Content: "hello there"},
			{Role: llm.RoleAssistant, Content: "hi"},
		},
		Runs: []sessionstore.Run{{
			ID:                 "run-1",
			Status:             sessionstore.RunCompleted,
			ModelContextWindow: 200000,
			Cost:               llm.CostInfo{TotalUSD: 0.42, Known: true},
			CreatedAt:          time.Unix(1700000000, 0),
			UpdatedAt:          time.Unix(1700000600, 0),
		}},
	}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	bus := event.New()
	// The logger is not installed in a test, and a refused request should not
	// be the reason one starts to be. note is neutralised for the same reason:
	// it is where an ordinary step reports itself, and a test that trips one
	// should not need a logger to survive it.
	previousWarn := warn
	previousNote := note
	warn = func(string) {}
	note = func(string) {}
	t.Cleanup(func() {
		warn = previousWarn
		note = previousNote
	})
	// Port 0, so the kernel hands the test a free port. The fixture used to leave
	// the address empty, which made every Start() in the package bind the real
	// 8787: two tests failed with "address already in use" whenever the service
	// was running, and the only way to get a green suite was to stop the bot.
	// That is a test defect, not a fact about the world, and it was written up in
	// AGENTS.md as if it were the latter for a day.
	return New(Config{Token: testToken, AllowID: allowID, Store: store, Bus: bus, Address: "127.0.0.1:0"}), store, bus
}

// do performs an authenticated request against the handler.
func do(t *testing.T, handler http.Handler, method, target, initData string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if initData != "" {
		req.Header.Set("Authorization", "tma "+initData)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestEnabledIsOnUnlessExplicitlyOff records the decision, and the shape of it.
//
// The surface used to require an explicit yes. It now requires an explicit no,
// which means these cases have to change together rather than one at a time: a
// value nobody wrote is a yes, and a misspelling is a yes, because there is no
// list of truthy spellings left to miss.
func TestEnabledIsOnUnlessExplicitlyOff(t *testing.T) {
	cases := map[string]bool{
		"":         true,
		"1":        true,
		"true":     true,
		"ON":       true,
		"yes":      true,
		"0":        false,
		"off":      false,
		"no":       false,
		"false":    false,
		"none":     false,
		"disabled": false,
		// The trade the inversion makes, stated as a case so it cannot be
		// rediscovered by surprise: an unrecognised setting now opens the port.
		"tru": true,
		"onx": true,
	}
	for value, want := range cases {
		t.Run(value, func(t *testing.T) {
			t.Setenv("CERAA_MINIAPP", value)
			if got := Enabled(); got != want {
				t.Fatalf("Enabled() with %q = %v, want %v", value, got, want)
			}
		})
	}
}

// TestEveryDataRouteRefusesARequestWithoutInitData covers the claim the plan
// rests on: nothing that carries data answers a caller with no credential, not
// even a path nobody registered.
//
// The shell is deliberately not in this list. Telegram delivers initData in the
// URL fragment, which the browser does not send, so the navigation that loads
// index.html cannot present one; refusing it would make every launch a bare 401.
// TestTheShellLoadsWithoutACredential covers that exception instead.
func TestEveryDataRouteRefusesARequestWithoutInitData(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	paths := []string{"/events", "/api/sessions", "/api/sessions/ses-fixture", "/api/unknown"}
	for _, path := range paths {
		if got := do(t, server.Handler(), http.MethodGet, path, ""); got.Code != http.StatusUnauthorized {
			t.Errorf("%s without init data = %d, want 401", path, got.Code)
		}
	}
}

// TestTheShellLoadsWithoutACredential pins the exception to the shell and
// nothing else.
//
// A regression here is silent in the way that matters most: the page 401s, the
// user sees the word "unauthorized" with no app around it, and the log has
// nothing to say because a missing fragment is deliberately not logged.
func TestTheShellLoadsWithoutACredential(t *testing.T) {
	serveAStubShell(t)
	server, _, _ := newFixture(t, testUser)
	// The shell itself must be served, because it cannot present a credential
	// until it has loaded and read the fragment it was opened with.
	if got := do(t, server.Handler(), http.MethodGet, "/", ""); got.Code != http.StatusOK {
		t.Errorf("/ without init data = %d, want 200: the page cannot read the credential it "+
			"was opened with unless it is served first", got.Code)
	}
	// Asking for it by name redirects to the directory, which is ServeFile
	// canonicalising rather than a refusal, so the bar here is only that it is
	// not turned away.
	if got := do(t, server.Handler(), http.MethodGet, "/index.html", ""); got.Code == http.StatusUnauthorized {
		t.Error("/index.html was refused without a credential, so the shell cannot be reached by name")
	}
}

// serveAStubShell puts an index.html where resolveAsset looks for it, so a test
// can assert on the response without depending on where the package is built.
func serveAStubShell(t *testing.T) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "miniapp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("stub shell dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><title>stub</title>"), 0o644); err != nil {
		t.Fatalf("stub shell: %v", err)
	}
	t.Setenv("CERAA_HOME", filepath.Dir(dir))
}

// TestTheShellReachesTheCredentialFromTheFragment is the client half of the
// same bug, asserted here so the two halves cannot drift apart.
//
// Telegram puts initData in the URL fragment. If index.html reads only
// location.search it finds nothing, never sets the Authorization header, and
// every call behind it is refused for the same reason the shell was exempted.
func TestTheShellReachesTheCredentialFromTheFragment(t *testing.T) {
	// Read the file that actually ships, not a stub: the bug this guards is in
	// the page that runs in Telegram, and a fixture cannot reproduce it.
	shell := filepath.Join("..", "..", "miniapp", "index.html")
	raw, err := os.ReadFile(shell)
	if err != nil {
		t.Fatalf("read shipped shell %s: %v", shell, err)
	}
	page := string(raw)
	for _, want := range []string{"tgWebAppData", "location.hash"} {
		if !strings.Contains(page, want) {
			t.Errorf("the shell never mentions %q, so it cannot read the fragment Telegram "+
				"puts the payload in; every launch would be refused", want)
		}
	}
	if strings.Contains(page, `var initData = params.get("initData") || "";`) {
		t.Error("the shell reads the payload from the query string alone, which is the bug this " +
			"test exists to keep fixed")
	}
}

// TestTheWrongUserIsForbidden keeps a genuine payload signed by this bot from
// opening the door to somebody the bot would not talk to.
func TestTheWrongUserIsForbidden(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	other := initDataFor(t, testToken, 999, 0)
	if got := do(t, server.Handler(), http.MethodGet, "/api/sessions", other); got.Code != http.StatusForbidden {
		t.Fatalf("another user = %d, want 403", got.Code)
	}
}

// TestAnExpiredPayloadIsRefused checks the freshness window is applied by the
// server and not only by Verify in isolation.
func TestAnExpiredPayloadIsRefused(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	stale := initDataFor(t, testToken, testUser, 48*time.Hour)
	got := do(t, server.Handler(), http.MethodGet, "/api/sessions", stale)
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("expired payload = %d, want 401: %s", got.Code, got.Body.String())
	}
}

// TestAcceptsInitDataFromTheQueryString covers the page load, which is a
// navigation and cannot carry a header the page has not loaded yet.
func TestAcceptsInitDataFromTheQueryString(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)
	target := "/api/sessions?initData=" + url.QueryEscape(initData)
	if got := do(t, server.Handler(), http.MethodGet, target, ""); got.Code != http.StatusOK {
		t.Fatalf("query-string init data = %d, want 200: %s", got.Code, got.Body.String())
	}
}

// TestSessionListAndDetail is the read-only phase's whole payload.
func TestSessionListAndDetail(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)

	list := do(t, server.Handler(), http.MethodGet, "/api/sessions", initData)
	if list.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", list.Code, list.Body.String())
	}
	var listed struct {
		Sessions []sessionSummary `json:"sessions"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(listed.Sessions))
	}
	if listed.Sessions[0].ID != "ses-fixture" || listed.Sessions[0].Messages != 2 {
		t.Fatalf("summary = %+v", listed.Sessions[0])
	}

	detail := do(t, server.Handler(), http.MethodGet, "/api/sessions/ses-fixture", initData)
	if detail.Code != http.StatusOK {
		t.Fatalf("detail = %d: %s", detail.Code, detail.Body.String())
	}
	var opened sessionDetail
	if err := json.Unmarshal(detail.Body.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}
	if len(opened.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(opened.Messages))
	}
	if opened.Context.Limit != 200000 {
		t.Errorf("context limit = %d, want 200000", opened.Context.Limit)
	}
	if opened.Context.EstimatedTokens <= 0 {
		t.Errorf("estimated tokens = %d, want above zero", opened.Context.EstimatedTokens)
	}
	if !opened.Cost.Known || opened.Cost.TotalUSD != 0.42 {
		t.Errorf("cost = %+v, want 0.42 and known", opened.Cost)
	}
}

// TestSystemMessagesAreNotTranscribed pins the line between what the model is
// told and what the chat shows.
//
// The system prompt is the first stored message of every session. Printing it
// hands the whole instruction set to whoever has the session open, and the
// surface only ever needed the conversation. It is withheld from the message
// list but still counted in the token estimate, because it sits in the context
// window and the estimate has to agree with what /status prints.
func TestSystemMessagesAreNotTranscribed(t *testing.T) {
	server, store, _ := newFixture(t, testUser)
	sess := &sessionstore.Session{
		ID:      "ses-system",
		Title:   "Has a system prompt",
		Version: 1,
		Messages: []sessionstore.Message{
			{Role: llm.RoleSystem, Content: "<core>secret instructions</core>"},
			{Role: llm.RoleUser, Content: "hello"},
			{Role: llm.RoleAssistant, Content: "hi"},
		},
	}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}

	detail := do(t, server.Handler(), http.MethodGet, "/api/sessions/ses-system",
		initDataFor(t, testToken, testUser, 0))
	if detail.Code != http.StatusOK {
		t.Fatalf("detail = %d: %s", detail.Code, detail.Body.String())
	}
	var opened sessionDetail
	if err := json.Unmarshal(detail.Body.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}
	if len(opened.Messages) != 2 {
		t.Fatalf("got %d messages, want 2 (system withheld)", len(opened.Messages))
	}
	for _, message := range opened.Messages {
		if message.Role == string(llm.RoleSystem) {
			t.Fatalf("system message reached the transcript: %+v", message)
		}
	}
	if strings.Contains(detail.Body.String(), "secret instructions") {
		t.Error("system prompt content is present in the response body")
	}
	// The estimate counts everything in the window, system included: the
	// secret prompt is 30+ characters, so dropping it would push the number
	// visibly below the 4-token floor the two-message conversation alone sets.
	if opened.Context.EstimatedTokens < len("<core>secret instructions</core>")/4 {
		t.Errorf("estimated tokens = %d, want the system message still counted",
			opened.Context.EstimatedTokens)
	}
}

// TestUnknownSessionIsNotFound separates a missing id from a refused caller.
func TestUnknownSessionIsNotFound(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)
	if got := do(t, server.Handler(), http.MethodGet, "/api/sessions/nope", initData); got.Code != http.StatusNotFound {
		t.Fatalf("unknown session = %d, want 404", got.Code)
	}
}

// TestResolveAssetRefusesNamesOutsideTheDirectory pins the gate directly,
// because a route test cannot reach a path the mux has already cleaned.
func TestResolveAssetRefusesNamesOutsideTheDirectory(t *testing.T) {
	for _, name := range []string{"../secret", "a/../../secret", "..", "/etc/passwd"} {
		if _, ok := resolveAsset(name); ok {
			t.Errorf("resolveAsset(%q) accepted a name outside the directory", name)
		}
	}
}

// TestEventsStreamsPublishedEvents covers the live half of the phase.
func TestEventsStreamsPublishedEvents(t *testing.T) {
	server, _, bus := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)

	live := httptest.NewServer(server.Handler())
	defer live.Close()

	req, err := http.NewRequest(http.MethodGet, live.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "tma "+initData)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream = %d, want 200", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	// The subscriber is running before the first byte is written, so reaching
	// the banner means it is safe to publish. The hello that follows is what
	// the page measures the transport by rather than the event under test, so
	// it is consumed here instead of being mistaken for one, blank line and
	// all: the next frame begins straight after it.
	var opening string
	for !(strings.Contains(opening, `"seq"`) && strings.HasSuffix(opening, "\n\n")) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading opening: %v", err)
		}
		opening += line
	}
	bus.Publish(event.Event{Type: event.TypePermission, SessionID: "ses-fixture", Data: map[string]any{"tool": "bash"}})

	var got string
	for i := 0; i < 3 && !strings.Contains(got, "data:"); i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading event: %v", err)
		}
		got += line
	}
	if !strings.Contains(got, "event: permission") || !strings.Contains(got, `"tool":"bash"`) {
		t.Fatalf("streamed frame = %q, want a permission event carrying the tool", got)
	}
}

// TestStartBindsLoopbackOnly is the posture claim: whatever port is asked for,
// the listener is on the loopback interface and nowhere else.
func TestStartBindsLoopbackOnly(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	if err := server.Start(); err != nil {
		// Port zero still needs a free port; a refusal here is environmental
		// rather than a defect in the code under test.
		t.Skipf("could not bind: %v", err)
	}
	defer server.Close()
	address := server.Address()
	if !strings.HasPrefix(address, "127.0.0.1:") && !strings.HasPrefix(address, "[::1]:") {
		t.Fatalf("bound %q, want a loopback address", address)
	}
}

// TestStartHonoursTheAddressOverride keeps a caller that needs a different port
// from having to edit the default.
func TestStartHonoursTheAddressOverride(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	server.cfg.Address = "127.0.0.1:0"
	if err := server.Start(); err != nil {
		t.Skipf("could not bind: %v", err)
	}
	defer server.Close()
	if server.Address() == "" {
		t.Fatal("Address is empty after Start")
	}
}

// TestCloseIsIdempotent guards the deferred chain, where a second close during
// shutdown is easy to write and would otherwise be an error.
func TestCloseIsIdempotent(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	if err := server.Close(); err != nil {
		t.Fatalf("Close before Start: %v", err)
	}
	if err := server.Start(); err != nil {
		t.Skipf("could not bind: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestMissingInitDataIsDistinguishedInLog separates the reason a request was
// refused without widening what the caller is told.
func TestMissingInitDataIsDistinguishedInLog(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	var said []string
	warn = func(message string) { said = append(said, message) }

	// No payload at all: nothing to report, because a browser pointed at the
	// port directly is not an incident.
	do(t, server.Handler(), http.MethodGet, "/api/sessions", "")
	if len(said) != 0 {
		t.Fatalf("logged %v for a request with no payload", said)
	}

	// A payload that does not verify: reported, because that is an attempt.
	bad := strings.Replace(initDataFor(t, testToken, testUser, 0), "auth_date=", "auth_date=1", 1)
	do(t, server.Handler(), http.MethodGet, "/api/sessions", bad)
	if len(said) != 1 {
		t.Fatalf("logged %v, want one line for a failed verification", said)
	}
}

// TestIndexIsServedFromDisk covers the one asset the page is, resolved the way
// the system prompt is: a copy in the data directory wins, and nothing is
// embedded to fall back on.
func TestIndexIsServedFromDisk(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CERAA_HOME", dataDir)
	page := filepath.Join(dataDir, "miniapp", "index.html")
	if err := os.MkdirAll(filepath.Dir(page), 0o755); err != nil {
		t.Fatal(err)
	}
	const body = "<!doctype html><title>Ceraa</title>"
	if err := os.WriteFile(page, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)
	got := do(t, server.Handler(), http.MethodGet, "/?initData="+url.QueryEscape(initData), "")
	if got.Code != http.StatusOK {
		t.Fatalf("index = %d, want 200", got.Code)
	}
	if !strings.Contains(got.Body.String(), "Ceraa") {
		t.Fatalf("index body = %q, want the page from disk", got.Body.String())
	}
}

// TestIndexIsRefusedWhenAbsent keeps a binary installed without its assets from
// pretending the page exists.
func TestIndexIsRefusedWhenAbsent(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)
	got := do(t, server.Handler(), http.MethodGet, "/?initData="+url.QueryEscape(initData), "")
	if got.Code != http.StatusNotFound {
		t.Fatalf("index = %d, want 404 when the page is missing", got.Code)
	}
}

// TestServesOverItsOwnSocket is the seam the other tests cannot reach: they
// drive the handler directly, which passes while Start wires nothing up. This
// one binds, dials and reads, so the chain from socket to stored session is the
// thing under test.
func TestServesOverItsOwnSocket(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	server.cfg.Address = "127.0.0.1:0"
	if err := server.Start(); err != nil {
		t.Skipf("could not bind: %v", err)
	}
	defer server.Close()

	initData := initDataFor(t, testToken, testUser, 0)
	req, err := http.NewRequest(http.MethodGet, "http://"+server.Address()+"/api/sessions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "tma "+initData)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("over the socket = %d, want 200: %s", resp.StatusCode, body)
	}
}

func postJSON(t *testing.T, handler http.Handler, target, initData, body string) *httptest.ResponseRecorder {
	return sendJSON(t, handler, http.MethodPost, target, initData, body)
}

func sendJSON(t *testing.T, handler http.Handler, method, target, initData, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if initData != "" {
		req.Header.Set("Authorization", "tma "+initData)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestPromptIsRefusedWithoutARunner keeps a surface that only ever read
// sessions from claiming it can start work.
func TestPromptIsRefusedWithoutARunner(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)
	got := postJSON(t, server.Handler(), "/prompt", initData, `{"text":"hello"}`)
	if got.Code != http.StatusServiceUnavailable {
		t.Fatalf("prompt without a runner = %d, want 503", got.Code)
	}
}

// TestPromptNeedsTheSameAdmissionAsEveryOtherPath is here because a route that
// starts work is the one somebody would try to reach without the check.
func TestPromptNeedsTheSameAdmissionAsEveryOtherPath(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	server.cfg.Prompt = func(context.Context, string) error { return nil }
	if got := postJSON(t, server.Handler(), "/prompt", "", `{"text":"hello"}`); got.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous prompt = %d, want 401", got.Code)
	}
	initData := initDataFor(t, testToken, 400000000, 0)
	if got := postJSON(t, server.Handler(), "/prompt", initData, `{"text":"hello"}`); got.Code != http.StatusForbidden {
		t.Fatalf("prompt as another user = %d, want 403", got.Code)
	}
}

// TestPromptRefusesInputsTheModelShouldNotHaveToSurvive pins the three ways a
// body can be wrong before any of them reaches a tokenizer.
func TestPromptRefusesInputsTheModelShouldNotHaveToSurvive(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	server.cfg.Prompt = func(context.Context, string) error { return nil }
	initData := initDataFor(t, testToken, testUser, 0)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"not json", "{{{", http.StatusBadRequest},
		{"no text", `{}`, http.StatusBadRequest},
		{"blank", `{"text":"   "}`, http.StatusBadRequest},
		{"too long", `{"text":"` + strings.Repeat("a", maxPromptChars+1) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := postJSON(t, server.Handler(), "/prompt", initData, tc.body)
			if got.Code != tc.want {
				t.Fatalf("%s = %d, want %d: %s", tc.name, got.Code, tc.want, got.Body.String())
			}
		})
	}
}

// TestPromptStreamsIntoItsOwnSession is the seam the plan cares about: the turn
// is taken, the answer leaves on the bus under the web session's id, and the
// reply says which conversation it landed in.
func TestPromptStreamsIntoItsOwnSession(t *testing.T) {
	server, _, bus := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)

	stream := bus.Subscribe(WebSessionID, 16)
	defer bus.Unsubscribe(WebSessionID, stream)
	// A stranger's session must not see this turn. Subscribing to it proves the
	// events are filed under the web id and nowhere else.
	other := bus.Subscribe("ses-fixture", 16)
	defer bus.Unsubscribe("ses-fixture", other)

	done := make(chan struct{})
	server.cfg.Prompt = func(_ context.Context, text string) error {
		defer close(done)
		if text != "write a test" {
			t.Errorf("runner received %q, want the message as sent", text)
		}
		bus.Publish(event.Event{Type: event.TypeContent, SessionID: WebSessionID, Data: map[string]any{"delta": "thinking "}})
		bus.Publish(event.Event{Type: event.TypeComplete, SessionID: WebSessionID, Data: map[string]any{"text": "thinking "}})
		return nil
	}

	got := postJSON(t, server.Handler(), "/prompt", initData, `{"text":"write a test"}`)
	if got.Code != http.StatusAccepted {
		t.Fatalf("prompt = %d, want 202: %s", got.Code, got.Body.String())
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the runner was never started")
	}

	var types []string
	timeout := time.After(2 * time.Second)
	for len(types) < 2 {
		select {
		case e, ok := <-stream:
			if !ok {
				t.Fatalf("stream closed early, saw %v", types)
			}
			types = append(types, string(e.Type))
		case <-timeout:
			t.Fatalf("saw %v after 2s, want a content delta and a completion", types)
		}
	}
	if types[0] != string(event.TypeContent) || types[1] != string(event.TypeComplete) {
		t.Fatalf("stream order = %v, want content then complete", types)
	}
	select {
	case e := <-other:
		t.Fatalf("the chat session received %s, but this turn is not its own", e.Type)
	default:
	}
}

// TestPromptRefusesASecondTurnWhileOneIsLive is the answer a queue would give,
// minus the queue: one transcript, one writer.
func TestPromptRefusesASecondTurnWhileOneIsLive(t *testing.T) {
	server, _, _ := newFixture(t, testUser)

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	server.cfg.Prompt = func(context.Context, string) error {
		started <- struct{}{}
		<-release
		return nil
	}
	initData := initDataFor(t, testToken, testUser, 0)

	if got := postJSON(t, server.Handler(), "/prompt", initData, `{"text":"one"}`); got.Code != http.StatusAccepted {
		t.Fatalf("first prompt = %d, want 202", got.Code)
	}
	<-started
	if got := postJSON(t, server.Handler(), "/prompt", initData, `{"text":"two"}`); got.Code != http.StatusConflict {
		t.Fatalf("second prompt = %d, want 409", got.Code)
	}

	close(release)
	// The gate is released after the runner returns, so there is a real window
	// here rather than a theoretical one.
	var again int
	for i := 0; i < 100; i++ {
		if again = postJSON(t, server.Handler(), "/prompt", initData, `{"text":"three"}`).Code; again == http.StatusAccepted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if again != http.StatusAccepted {
		t.Fatalf("prompt after the first run finished = %d, want 202", again)
	}
}

// TestDecisionsAreRefusedWithoutAWiring keeps an admitted reader from being
// told a decision was recorded when the process never gave the server anywhere
// to record it.
func TestDecisionsAreRefusedWithoutAWiring(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)

	got := postJSON(t, server.Handler(), "/api/permission", initData, `{"request_id":"r1","decision":"deny"}`)
	if got.Code != http.StatusServiceUnavailable {
		t.Fatalf("permission with no wiring = %d, want 503", got.Code)
	}
	got = postJSON(t, server.Handler(), "/api/answer", initData, `{"request_id":"q1","answer":"yes"}`)
	if got.Code != http.StatusServiceUnavailable {
		t.Fatalf("answer with no wiring = %d, want 503", got.Code)
	}
}

func TestDecisionsNeedTheSameAdmissionAsEveryOtherPath(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	server.cfg.AnswerPermission = func(string, string) bool { return true }
	server.cfg.AnswerQuestion = func(string, string) bool { return true }
	for _, target := range []string{"/api/permission", "/api/answer"} {
		if got := postJSON(t, server.Handler(), target, "", `{"request_id":"r1"}`); got.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s = %d, want 401", target, got.Code)
		}
	}
}

// TestPermissionForwardsOnlyAVocabularyDecision pins that a request body cannot
// talk the manager into accepting a string it was never asked to understand.
func TestPermissionForwardsOnlyAVocabularyDecision(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)
	var gotID, gotDecision string
	server.cfg.AnswerPermission = func(requestID, decision string) bool {
		gotID, gotDecision = requestID, decision
		return true
	}

	bad := []struct {
		name string
		body string
	}{
		{"unknown decision", `{"request_id":"req-1","decision":"maybe"}`},
		{"empty decision", `{"request_id":"req-1","decision":""}`},
		{"empty request id", `{"request_id":"","decision":"deny"}`},
		{"not json", `nope`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if got := postJSON(t, server.Handler(), "/api/permission", initData, tc.body); got.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d, want 400: %s", tc.name, got.Code, got.Body.String())
			}
		})
	}
	if gotID != "" || gotDecision != "" {
		t.Fatalf("a refused request reached the manager as %q/%q", gotID, gotDecision)
	}

	ok := postJSON(t, server.Handler(), "/api/permission", initData, `{"request_id":"req-1","decision":"allowOnce"}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("valid permission = %d, want 200: %s", ok.Code, ok.Body.String())
	}
	if gotID != "req-1" || gotDecision != "allowOnce" {
		t.Fatalf("forwarded %q/%q, want req-1/allowOnce", gotID, gotDecision)
	}
}

// TestDecisionsRefuseARequestThatIsGone is the answer a manager gives when the
// request has already been answered from the chat or timed out. Fabricating a
// 200 there would tell a user their click landed when nothing was waiting.
func TestDecisionsRefuseARequestThatIsGone(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)
	server.cfg.AnswerPermission = func(string, string) bool { return false }
	server.cfg.AnswerQuestion = func(string, string) bool { return false }

	got := postJSON(t, server.Handler(), "/api/permission", initData, `{"request_id":"req-1","decision":"deny"}`)
	if got.Code != http.StatusConflict {
		t.Fatalf("stale permission = %d, want 409", got.Code)
	}
	got = postJSON(t, server.Handler(), "/api/answer", initData, `{"request_id":"q1","answer":"ship"}`)
	if got.Code != http.StatusConflict {
		t.Fatalf("stale answer = %d, want 409", got.Code)
	}
}

// TestAnswerForwardsTheTextUntouched covers the one thing this surface is
// trusted with: passing on what was typed, not deciding what it means.
func TestAnswerForwardsTheTextUntouched(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)
	var gotID, gotAnswer string
	server.cfg.AnswerQuestion = func(requestID, answer string) bool {
		gotID, gotAnswer = requestID, answer
		return true
	}

	if got := postJSON(t, server.Handler(), "/api/answer", initData, `{"request_id":"","answer":"ship"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("empty request id = %d, want 400", got.Code)
	}
	if got := postJSON(t, server.Handler(), "/api/answer", initData, `{"request_id":"q1","answer":"   "}`); got.Code != http.StatusBadRequest {
		t.Fatalf("blank answer = %d, want 400", got.Code)
	}
	ok := postJSON(t, server.Handler(), "/api/answer", initData, `{"request_id":"q1","answer":"/tmp/thing.go"}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("answer = %d, want 200: %s", ok.Code, ok.Body.String())
	}
	if gotID != "q1" || gotAnswer != "/tmp/thing.go" {
		t.Fatalf("forwarded %q/%q, want q1//tmp/thing.go", gotID, gotAnswer)
	}
}

// TestSettingsRoundTrip covers the pair of verbs as one contract: a write is
// only meaningful if the read afterwards shows it.
func TestSettingsRoundTrip(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)

	current := map[string]any{"model": "a", "agent": "build"}
	server.cfg.Settings = func() map[string]any { return current }
	server.cfg.SetSettings = func(update map[string]any) error {
		for k, v := range update {
			current[k] = v
		}
		return nil
	}

	got := do(t, server.Handler(), http.MethodGet, "/api/settings", initData)
	if got.Code != http.StatusOK {
		t.Fatalf("read = %d, want 200", got.Code)
	}
	if !strings.Contains(got.Body.String(), `"model":"a"`) {
		t.Fatalf("read body = %s, want the current snapshot", got.Body.String())
	}

	saved := postJSON(t, server.Handler(), "/api/settings", initData, `{"settings":{"model":"b"}}`)
	if saved.Code != http.StatusOK {
		t.Fatalf("write = %d, want 200: %s", saved.Code, saved.Body.String())
	}
	if current["model"] != "b" {
		t.Fatalf("model = %v, want b", current["model"])
	}
	if !strings.Contains(saved.Body.String(), `"model":"b"`) {
		t.Fatalf("write body = %s, want the snapshot after the change", saved.Body.String())
	}
}

// TestSettingsRefuseToHalfSave keeps a malformed body from applying the keys it
// happened to understand.
func TestSettingsRefuseToHalfSave(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)
	var applied map[string]any
	server.cfg.Settings = func() map[string]any { return map[string]any{} }
	server.cfg.SetSettings = func(update map[string]any) error {
		applied = update
		return fmt.Errorf(`unknown setting "model"`)
	}

	for _, body := range []string{`{}`, `{"settings":{}}`, `not json`, `{"settings":[]}`} {
		if got := postJSON(t, server.Handler(), "/api/settings", initData, body); got.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", body, got.Code)
		}
	}
	if applied != nil {
		t.Fatalf("a refused body reached the applier as %v", applied)
	}

	if got := postJSON(t, server.Handler(), "/api/settings", initData, `{"settings":{"model":"x"}}`); got.Code != http.StatusBadRequest {
		t.Fatalf("domain rejection = %d, want 400", got.Code)
	}
}

func TestSettingsAreRefusedWithoutAWiring(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)
	if got := do(t, server.Handler(), http.MethodGet, "/api/settings", initData); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("read without wiring = %d, want 503", got.Code)
	}
}

// TestSecretsRoundTripNeverRevealsAValue is the whole reason the store exists:
// this surface can put a credential in and take one out, and cannot read any of
// them back at any point.
func TestSecretsRoundTripNeverRevealsAValue(t *testing.T) {
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)

	stored := sendJSON(t, server.Handler(), http.MethodPut, "/api/secrets/OPENAI_KEY", initData, `{"value":"sk-super-secret-value"}`)
	if stored.Code != http.StatusOK {
		t.Fatalf("put = %d, want 200: %s", stored.Code, stored.Body.String())
	}
	if strings.Contains(stored.Body.String(), "sk-super-secret-value") {
		t.Fatalf("the store echoed the value back: %s", stored.Body.String())
	}

	listed := do(t, server.Handler(), http.MethodGet, "/api/secrets", initData)
	if listed.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", listed.Code)
	}
	body := listed.Body.String()
	if strings.Contains(body, "sk-super-secret-value") {
		t.Fatalf("the list leaked the value: %s", body)
	}
	if !strings.Contains(body, "OPENAI_KEY") {
		t.Fatalf("list body = %s, want the stored name", body)
	}

	// Written, so it decrypts the way the chat handlers read it.
	value, err := secret.Get("OPENAI_KEY")
	if err != nil || value != "sk-super-secret-value" {
		t.Fatalf("store read back %q/%v, want the value as written", value, err)
	}

	deleted := do(t, server.Handler(), http.MethodDelete, "/api/secrets/OPENAI_KEY", initData)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete = %d, want 200", deleted.Code)
	}
	if got := do(t, server.Handler(), http.MethodDelete, "/api/secrets/OPENAI_KEY", initData); got.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", got.Code)
	}
}

// TestSecretsRefuseTheNamesThisProcessOwns keeps a page from overwriting a
// credential the bot uses to authenticate itself.
func TestSecretsRefuseTheNamesThisProcessOwns(t *testing.T) {
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)

	for _, name := range []string{"ceraa_bot_token", "provider_api_key_openai"} {
		got := sendJSON(t, server.Handler(), http.MethodPut, "/api/secrets/"+name, initData, `{"value":"x"}`)
		if got.Code != http.StatusForbidden {
			t.Fatalf("put %s = %d, want 403", name, got.Code)
		}
	}
	if got := sendJSON(t, server.Handler(), http.MethodPut, "/api/secrets/too-long-"+strings.Repeat("a", 80), initData, `{"value":"x"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("overlong name = %d, want 400", got.Code)
	}
}

func TestSecretWritesNeedAdmission(t *testing.T) {
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	server, _, _ := newFixture(t, testUser)
	if got := do(t, server.Handler(), http.MethodGet, "/api/secrets", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list = %d, want 401", got.Code)
	}
	if got := sendJSON(t, server.Handler(), http.MethodPut, "/api/secrets/KEY", "", `{"value":"x"}`); got.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous put = %d, want 401", got.Code)
	}
}

// TestCancelRefusesAnotherSessionsRun keeps this surface from appearing to
// reach a conversation it has no handle on.
func TestCancelRefusesAnotherSessionsRun(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)
	for _, id := range []string{"ses-fixture", "ses-something-else", WebSessionID + "-typo"} {
		got := postJSON(t, server.Handler(), "/api/sessions/"+id+"/cancel", initData, `{}`)
		if got.Code != http.StatusNotFound {
			t.Fatalf("cancel %q = %d, want 404", id, got.Code)
		}
	}
}

func TestCancelWithNothingRunningIsAConflict(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)
	if got := postJSON(t, server.Handler(), "/api/sessions/"+WebSessionID+"/cancel", initData, `{}`); got.Code != http.StatusConflict {
		t.Fatalf("cancel with nothing running = %d, want 409", got.Code)
	}
}

// TestCancelStopsTheRunAndReportsNoFailure is the whole of the last phase: the
// run actually stops, the gate opens again, and a cancellation is not filed as
// a failure to whoever is watching.
func TestCancelStopsTheRunAndReportsNoFailure(t *testing.T) {
	server, _, bus := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)

	stream := bus.Subscribe(WebSessionID, 16)
	defer bus.Unsubscribe(WebSessionID, stream)

	stopped := make(chan struct{})
	server.cfg.Prompt = func(ctx context.Context, text string) error {
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}

	if got := postJSON(t, server.Handler(), "/prompt", initData, `{"text":"long job"}`); got.Code != http.StatusAccepted {
		t.Fatalf("prompt = %d, want 202", got.Code)
	}
	if got := postJSON(t, server.Handler(), "/api/sessions/"+WebSessionID+"/cancel", initData, `{}`); got.Code != http.StatusOK {
		t.Fatalf("cancel = %d, want 200: %s", got.Code, got.Body.String())
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the run did not stop")
	}

	// Nothing may be filed as a failure, and the gate must open again. Both
	// happen after the runner returns, so this waits rather than assumes.
	deadline := time.After(2 * time.Second)
	for {
		if got := postJSON(t, server.Handler(), "/prompt", initData, `{"text":"after"}`); got.Code == http.StatusAccepted {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the run never finished, or the gate never reopened")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case e := <-stream:
		if e.Type == event.TypeError {
			t.Fatalf("a cancelled run was reported as a failure: %v", e.Data)
		}
	default:
	}
}

// The shell is edited on disk and must appear on the next open. ServeFile
// otherwise sends Last-Modified and answers 304 from If-Modified-Since, and a
// WebView holding that chain can keep rendering an older revision indefinitely.
// That presents as a fix that did not work: the file is right on disk and the
// phone disagrees, with nothing in either log to explain it.
func TestTheShellIsNotCached(t *testing.T) {
	serveAStubShell(t)
	server, _, _ := newFixture(t, testUser)
	got := do(t, server.Handler(), http.MethodGet, "/", "")
	if got.Code != http.StatusOK {
		t.Fatalf("/ = %d, want 200", got.Code)
	}
	if cache := got.Header().Get("Cache-Control"); !strings.Contains(cache, "no-store") {
		t.Errorf("Cache-Control = %q, want it to forbid storing: an edited shell has to reach the "+
			"phone on the next open", cache)
	}
}

// The session sheet must not be able to cover the conversation. Hiding it with a
// transform alone means any client that drops that one property — or a
// stylesheet that fails to parse it — leaves a full-height panel over the screen
// with the control that opens it hidden behind. Visibility is the guarantee;
// the transform is only how it arrives.
func TestTheClosedSheetCannotCoverTheConversation(t *testing.T) {
	// The shipped file, not a stub: the defect is in the page that runs.
	shell := filepath.Join("..", "..", "miniapp", "index.html")
	raw, err := os.ReadFile(shell)
	if err != nil {
		t.Fatalf("read shipped shell %s: %v", shell, err)
	}
	page := string(raw)
	if !strings.Contains(page, "visibility: hidden") {
		t.Error("the closed sheet is not hidden by visibility, so it depends on one property to stay " +
			"off screen")
	}
	if !strings.Contains(page, "visibility: visible") {
		t.Error("the open state never restores visibility, so the sheet could not be opened at all")
	}
}

// The script the Mini App runs has to be syntactically whole and has to keep the
// functions the surface depends on.
//
// A redesign that copies the script out of the page can quietly lose its last
// line. An unterminated IIFE is a syntax error, so the whole block never runs:
// no credential is read, no listener is attached, and the page is inert while
// still looking correct in a screenshot. The evidence is structural rather than
// visual, so it is asserted structurally.
func TestTheShippedScriptIsWhole(t *testing.T) {
	shell := filepath.Join("..", "..", "miniapp", "index.html")
	raw, err := os.ReadFile(shell)
	if err != nil {
		t.Fatalf("read shipped shell %s: %v", shell, err)
	}
	page := string(raw)

	for _, want := range []string{
		"attach();",         // wiring happens
		"})();",             // and the block it lives in is closed
		"/prompt",           // sending a task
		"/events?initData=", // streaming
		"/api/permission",   // answering a permission
		"/api/sessions",     // the session list
		"/api/secrets",      // and the settings surface
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the shipped shell no longer contains %q; the script is not whole and the "+
				"surface will be inert", want)
		}
	}

	// Balanced braces catch the same class of truncation anywhere in the block.
	blocks := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(page, -1)
	if len(blocks) == 0 {
		t.Fatal("the shell has no script")
	}
	for i, b := range blocks {
		body := b[1]
		if strings.Count(body, "{") != strings.Count(body, "}") {
			t.Errorf("script block %d has %d { and %d }, so it is truncated",
				i, strings.Count(body, "{"), strings.Count(body, "}"))
		}
	}
}
