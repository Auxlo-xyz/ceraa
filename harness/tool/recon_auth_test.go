package tool

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/jar"
	"ceraa/harness/recon"
)

// The auth tools are a login, a replay and a listing. These tests cover the
// argument shapes and the error the model can act on. The one test that
// sends a request uses httptest, with the scope and the guard opened for the
// loopback address the test server listens on.

func TestAuthLoginTranslateRejectsAMissingNameOrURL(t *testing.T) {
	tool := &AuthLoginTool{}
	for _, args := range []map[string]any{
		{},
		{"url": "https://example.com/login"},
		{"name": "admin"},
		{"name": "", "url": "https://example.com/login"},
		{"name": "admin", "url": ""},
		{"name": "admin", "url": "not a url"},
		{"name": "admin", "url": "ftp://example.com/login"},
		{"name": "admin", "url": "/login"},
		{"name": "../escape", "url": "https://example.com/login"},
	} {
		if spec, err := tool.Translate(args); err == nil {
			t.Errorf("Translate(%v) accepted, spec = %v", args, spec)
		}
	}
}

func TestAuthReplayTranslateRejectsAMissingSessionOrURL(t *testing.T) {
	tool := &AuthReplayTool{}
	for _, args := range []map[string]any{
		{},
		{"url": "https://example.com/admin"},
		{"session": "admin"},
		{"session": "", "url": "https://example.com/admin"},
		{"session": "admin", "url": ""},
		{"session": "../escape", "url": "https://example.com/admin"},
	} {
		if spec, err := tool.Translate(args); err == nil {
			t.Errorf("Translate(%v) accepted, spec = %v", args, spec)
		}
	}
}

// The arguments arrive from JSON, so an int is a float64 and a body is
// either a string or an object. Both have to arrive at the request as the
// shape the probe expects.
func TestAuthTranslateNormalisesAFloatTimeoutAndAnObjectBody(t *testing.T) {
	tool := &AuthLoginTool{}
	spec, err := tool.Translate(map[string]any{
		"name":    "admin",
		"url":     "https://example.com/login",
		"method":  "post",
		"body":    map[string]any{"user": "admin", "password": "hunter2"},
		"headers": map[string]any{"X-Count": 1, "Authorization": "Bearer abc"},
		"timeout": float64(30),
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if spec.Type != "auth_login" {
		t.Errorf("spec.Type = %q, want auth_login", spec.Type)
	}
	if method, _ := spec.Args["method"].(string); method != "POST" {
		t.Errorf("method = %v, want POST", spec.Args["method"])
	}
	if timeout, _ := spec.Args["timeout"].(int); timeout != 30 {
		t.Errorf("timeout = %v, want the 30 of the float64", spec.Args["timeout"])
	}
	body, _ := spec.Args["body"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("body is not JSON: %q", body)
	}
	if payload["user"] != "admin" || payload["password"] != "hunter2" {
		t.Errorf("payload = %v", payload)
	}
	headers, _ := spec.Args["headers"].(map[string]string)
	if headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type = %q, want the JSON default", headers["Content-Type"])
	}
	if headers["X-Count"] != "1" {
		t.Errorf("X-Count = %q, want the coerced string", headers["X-Count"])
	}
	if headers["Authorization"] != "Bearer abc" {
		t.Errorf("Authorization = %q", headers["Authorization"])
	}

	replay := &AuthReplayTool{}
	spec, err = replay.Translate(map[string]any{
		"session": "admin",
		"url":     "https://example.com/admin",
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if spec.Type != "auth_replay" {
		t.Errorf("spec.Type = %q, want auth_replay", spec.Type)
	}
	if method, _ := spec.Args["method"].(string); method != "GET" {
		t.Errorf("default method = %v, want GET", spec.Args["method"])
	}
	if timeout, _ := spec.Args["timeout"].(int); timeout != 15 {
		t.Errorf("default timeout = %v, want 15", spec.Args["timeout"])
	}
}

// A missing session is a name the model can fix, so the error says which
// names exist.
func TestAMissingSessionNamesTheStoredSessions(t *testing.T) {
	dir := t.TempDir()
	withAuthSessionDir(t, dir)

	store, err := jar.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, name := range []string{"admin", "user"} {
		if err := store.Save(jar.Session{Name: name}); err != nil {
			t.Fatalf("Save %q: %v", name, err)
		}
	}

	tool := &AuthReplayTool{}
	spec, err := tool.Translate(map[string]any{
		"session": "missing",
		"url":     "https://example.com/admin",
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	_, err = tool.RunContext(context.Background(), spec)
	if err == nil {
		t.Fatal("a replay of a session that is not there succeeded")
	}
	message := err.Error()
	if !strings.Contains(message, "missing") {
		t.Errorf("error = %q, want it to name the session asked for", message)
	}
	if !strings.Contains(message, "admin") || !strings.Contains(message, "user") {
		t.Errorf("error = %q, want it to name the sessions that exist", message)
	}
}

// The listing is a catalogue: names and cookie names, never values.
func TestTheSessionListingCarriesNoCookieValue(t *testing.T) {
	dir := t.TempDir()
	withAuthSessionDir(t, dir)

	store, err := jar.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Save(jar.Session{
		Name: "admin",
		Cookies: []jar.Cookie{
			{Name: "sessionid", Value: "s3cr3t-value", Domain: "example.com", Path: "/"},
		},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	tool := &AuthSessionsTool{}
	spec, err := tool.Translate(map[string]any{})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}
	if !strings.Contains(out, "admin") || !strings.Contains(out, "sessionid") {
		t.Errorf("listing = %q, want the name and the cookie name", out)
	}
	if strings.Contains(out, "s3cr3t-value") {
		t.Errorf("listing leaked a cookie value: %q", out)
	}
}

func TestTheSessionListingDeletesOnRequest(t *testing.T) {
	dir := t.TempDir()
	withAuthSessionDir(t, dir)

	store, err := jar.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Save(jar.Session{Name: "admin"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	tool := &AuthSessionsTool{}
	spec, err := tool.Translate(map[string]any{"mode": "delete", "name": "admin"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}
	if !strings.Contains(out, "admin") || !strings.Contains(out, "deleted") {
		t.Errorf("output = %q, want it to say the session was deleted", out)
	}
	if _, err := store.Load("admin"); err == nil {
		t.Error("the session file survived the delete")
	}
}

// The end to end path: a login against a test server stores the cookies the
// response set, and the answer names them without printing a value or the
// body.
func TestALoginStoresTheSessionAndPrintsNoValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "sessionid=s3cr3t-cookie-value; Path=/; HttpOnly")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"token":"body-secret-value"}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	withAuthSessionDir(t, dir)
	allowLoopback(t, srv)

	tool := &AuthLoginTool{}
	spec, err := tool.Translate(map[string]any{
		"name": "admin",
		"url":  srv.URL + "/login",
		"body": map[string]any{"user": "admin", "password": "hunter2"},
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}
	if !strings.Contains(out, "status 200") {
		t.Errorf("output = %q, want the status", out)
	}
	if !strings.Contains(out, "sessionid") {
		t.Errorf("output = %q, want the cookie name", out)
	}
	if strings.Contains(out, "s3cr3t-cookie-value") {
		t.Errorf("output leaked a cookie value: %q", out)
	}
	if strings.Contains(out, "body-secret-value") {
		t.Errorf("output leaked the response body: %q", out)
	}
	if !strings.Contains(out, filepath.Join(dir, "admin.json")) {
		t.Errorf("output = %q, want the session file path", out)
	}

	store, err := jar.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sess, err := store.Load("admin")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(sess.Cookies) != 1 {
		t.Fatalf("cookies = %d, want 1", len(sess.Cookies))
	}
	cookie := sess.Cookies[0]
	if cookie.Name != "sessionid" || cookie.Value != "s3cr3t-cookie-value" {
		t.Errorf("cookie = %+v", cookie)
	}
	if cookie.Path != "/" {
		t.Errorf("path = %q, want /", cookie.Path)
	}
	if !cookie.HTTPOnly {
		t.Error("the HttpOnly flag was lost")
	}
}

// The coordinator validates the arguments against the schema before the tool
// ever sees them, so a schema that refuses a legal call costs a retry.
func TestTheAuthSchemasAcceptTheArguments(t *testing.T) {
	login := &AuthLoginTool{}
	if err := ValidateArguments(login.Schema(), map[string]any{
		"name":    "admin",
		"url":     "https://example.com/login",
		"method":  "POST",
		"body":    map[string]any{"user": "admin", "password": "hunter2"},
		"headers": map[string]any{"Content-Type": "application/json", "X-Count": 1},
		"timeout": 15,
	}); err != nil {
		t.Fatalf("the login schema refused a legal call: %v", err)
	}

	replay := &AuthReplayTool{}
	if err := ValidateArguments(replay.Schema(), map[string]any{
		"session": "admin",
		"url":     "https://example.com/admin",
		"body":    map[string]any{"id": 42},
	}); err != nil {
		t.Fatalf("the replay schema refused a legal call: %v", err)
	}

	sessions := &AuthSessionsTool{}
	if err := ValidateArguments(sessions.Schema(), map[string]any{"mode": "list"}); err != nil {
		t.Fatalf("the sessions schema refused a list call: %v", err)
	}
	if err := ValidateArguments(sessions.Schema(), map[string]any{"mode": "delete", "name": "admin"}); err != nil {
		t.Fatalf("the sessions schema refused a delete call: %v", err)
	}
	if err := ValidateArguments(sessions.Schema(), map[string]any{"mode": "burn"}); err == nil {
		t.Error("the sessions schema accepted an unknown mode")
	}
}

// withAuthSessionDir points the tools at a scratch directory for the
// duration of a test, and puts the old one back afterwards.
func withAuthSessionDir(t *testing.T, dir string) {
	t.Helper()
	previous := authSessionDir
	authSessionDir = dir
	t.Cleanup(func() { authSessionDir = previous })
}

// allowLoopback opens the two checks a test server needs: the scope record
// that authorizes the loopback address as an explicit internal target, and
// the netguard exception that lets the dial reach it.
//
// The scope record is the operator's own authorization, so it is written
// through the same call the CLI uses, and the whole data directory is
// pointed at a temporary one so the live authorization file is untouched.
func allowLoopback(t *testing.T, srv *httptest.Server) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CERAA_HOME", home)
	t.Setenv("CERAA_LOG_FILE", filepath.Join(home, "bridge.log"))
	t.Setenv("CERAA_ALLOW_PRIVATE_NET", "127.0.0.1")

	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split the test address: %v", err)
	}
	if err := recon.SetActive(recon.Engagement{
		InScope: []string{net.JoinHostPort(host, port)},
	}); err != nil {
		t.Fatalf("set the engagement: %v", err)
	}
	t.Cleanup(func() {
		recon.Stop()
		os.RemoveAll(home)
	})
}
