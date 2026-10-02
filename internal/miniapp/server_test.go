package miniapp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/harness/event"
	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
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
	// be the reason one starts to be.
	previousWarn := warn
	warn = func(string) {}
	t.Cleanup(func() { warn = previousWarn })
	return New(Config{Token: testToken, AllowID: allowID, Store: store, Bus: bus}), store, bus
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

// TestEnabledFailsClosedOnATypo is the whole reason Enabled is written the way
// it is. This variable opens a port, so anything not explicitly asked for must
// leave it shut.
func TestEnabledFailsClosedOnATypo(t *testing.T) {
	cases := map[string]bool{
		"1":    true,
		"true": true,
		"ON":   true,
		"":     false,
		"0":    false,
		"off":  false,
		"tru":  false,
		"onx":  false,
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

// TestEveryRouteRefusesARequestWithoutInitData covers the claim the plan rests
// on: there is no unauthenticated surface, not even a path nobody registered.
func TestEveryRouteRefusesARequestWithoutInitData(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	paths := []string{"/", "/events", "/api/sessions", "/api/sessions/ses-fixture", "/api/unknown", "/index.html"}
	for _, path := range paths {
		if got := do(t, server.Handler(), http.MethodGet, path, ""); got.Code != http.StatusUnauthorized {
			t.Errorf("%s without init data = %d, want 401", path, got.Code)
		}
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
	// The subscription happens before the first byte is written, so reaching
	// the banner means it is safe to publish.
	for i := 0; i < 2; i++ {
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatalf("reading banner: %v", err)
		}
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
