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

// The auth tool is one tool with four modes: a login, a replay, a listing and
// a delete. These tests cover the argument shapes per mode and the errors the
// model can act on. The two tests that send a request use httptest, with the
// scope and the guard opened for the loopback address the test server listens
// on.

// The mode decides what the call must carry, so a call that is short of an
// argument is refused per mode rather than by a schema that cannot express it.
func TestAuthTranslateRefusesWhatTheModeNeeds(t *testing.T) {
	tool := &AuthTool{}
	for _, args := range []map[string]any{
		// No mode is a login, and a login needs a name and a url.
		{},
		{"mode": "login"},
		{"mode": "login", "url": "https://example.com/login"},
		{"mode": "login", "name": "admin"},
		{"mode": "login", "name": "", "url": "https://example.com/login"},
		{"mode": "login", "name": "admin", "url": ""},
		{"mode": "login", "name": "admin", "url": "not a url"},
		{"mode": "login", "name": "admin", "url": "ftp://example.com/login"},
		{"mode": "login", "name": "admin", "url": "/login"},
		{"mode": "login", "name": "../escape", "url": "https://example.com/login"},
		// A replay needs a session and a url.
		{"mode": "replay"},
		{"mode": "replay", "url": "https://example.com/admin"},
		{"mode": "replay", "session": "admin"},
		{"mode": "replay", "session": "", "url": "https://example.com/admin"},
		{"mode": "replay", "session": "admin", "url": ""},
		{"mode": "replay", "session": "../escape", "url": "https://example.com/admin"},
		// A delete needs a name.
		{"mode": "delete"},
		{"mode": "delete", "name": ""},
		{"mode": "delete", "name": "../escape"},
		// An unknown mode is not a mode.
		{"mode": "burn"},
	} {
		if spec, err := tool.Translate(args); err == nil {
			t.Errorf("Translate(%v) accepted, spec = %v", args, spec)
		}
	}
}

// The error names the argument that was missing, because the mode decides
// which one that was.
func TestAuthRefusesTheMissingArgumentByName(t *testing.T) {
	tool := &AuthTool{}
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"mode": "login", "url": "https://example.com/login"}, "name is required"},
		{map[string]any{"mode": "login", "name": "admin"}, "url is required"},
		{map[string]any{"mode": "replay", "url": "https://example.com/admin"}, "session is required"},
		{map[string]any{"mode": "replay", "session": "admin"}, "url is required"},
		{map[string]any{"mode": "delete"}, "name is required"},
	} {
		_, err := tool.Translate(tc.args)
		if err == nil {
			t.Errorf("Translate(%v) accepted", tc.args)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Translate(%v) error = %q, want it to say %q", tc.args, err, tc.want)
		}
	}
}

// The arguments arrive from JSON, so an int is a float64 and a body is
// either a string or an object. Both have to arrive at the request as the
// shape the probe expects.
func TestAuthTranslateNormalisesAFloatTimeoutAndAnObjectBody(t *testing.T) {
	tool := &AuthTool{}
	spec, err := tool.Translate(map[string]any{
		"mode":    "login",
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
	if spec.Type != "auth" {
		t.Errorf("spec.Type = %q, want auth", spec.Type)
	}
	if mode, _ := spec.Args["mode"].(string); mode != "login" {
		t.Errorf("mode = %v, want login", spec.Args["mode"])
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

	spec, err = tool.Translate(map[string]any{
		"mode":    "replay",
		"session": "admin",
		"url":     "https://example.com/admin",
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if session, _ := spec.Args["session"].(string); session != "admin" {
		t.Errorf("session = %v, want admin", spec.Args["session"])
	}
	if method, _ := spec.Args["method"].(string); method != "GET" {
		t.Errorf("default method = %v, want GET", spec.Args["method"])
	}
	if timeout, _ := spec.Args["timeout"].(int); timeout != 15 {
		t.Errorf("default timeout = %v, want 15", spec.Args["timeout"])
	}
}

// The mode defaults to login: the tool exists to keep a session, and a login
// is what starts one.
func TestTheAuthModeDefaultsToLogin(t *testing.T) {
	mode, err := authMode(nil)
	if err != nil {
		t.Fatalf("authMode: %v", err)
	}
	if mode != "login" {
		t.Errorf("default mode = %q, want login", mode)
	}
}

// The four modes are the whole tool, and the schema says which one it is.
func TestTheAuthSchemaNamesTheFourModes(t *testing.T) {
	properties, _ := (&AuthTool{}).Schema()["properties"].(map[string]any)
	mode, _ := properties["mode"].(map[string]any)
	enum, _ := mode["enum"].([]string)
	want := []string{"login", "replay", "list", "delete"}
	if len(enum) != len(want) {
		t.Fatalf("enum = %v, want %v", enum, want)
	}
	for i, name := range want {
		if enum[i] != name {
			t.Errorf("enum[%d] = %q, want %q", i, enum[i], name)
		}
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

	tool := &AuthTool{}
	spec, err := tool.Translate(map[string]any{
		"mode":    "replay",
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

	tool := &AuthTool{}
	spec, err := tool.Translate(map[string]any{"mode": "list"})
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

func TestTheDeleteModeRemovesTheSession(t *testing.T) {
	dir := t.TempDir()
	withAuthSessionDir(t, dir)

	store, err := jar.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Save(jar.Session{Name: "admin"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	tool := &AuthTool{}
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

	tool := &AuthTool{}
	spec, err := tool.Translate(map[string]any{
		"mode": "login",
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

// The replay is a normal probe: it carries the session's cookies, the
// caller's headers win over the stored ones, and the observation lands in the
// receipt trail.
func TestAReplaySendsTheSessionAndTheCallersHeaders(t *testing.T) {
	var cookie, authorization, caller string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie = r.Header.Get("Cookie")
		authorization = r.Header.Get("Authorization")
		caller = r.Header.Get("X-Caller")
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	dir := t.TempDir()
	withAuthSessionDir(t, dir)
	// The receipt lands under the data directory the recon package resolved
	// at start-up, which this test cannot redirect, so the path is read
	// before the environment is pointed elsewhere.
	receipts := reconReceiptsDir()
	allowLoopback(t, srv)

	store, err := jar.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Save(jar.Session{
		Name: "admin",
		Cookies: []jar.Cookie{
			{Name: "sessionid", Value: "s3cr3t-cookie-value", Domain: "127.0.0.1", Path: "/"},
		},
		Headers: map[string]string{"Authorization": "Bearer stored"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	tool := &AuthTool{}
	spec, err := tool.Translate(map[string]any{
		"mode":    "replay",
		"session": "admin",
		"url":     srv.URL + "/admin",
		"headers": map[string]any{"Authorization": "Bearer caller", "X-Caller": "yes"},
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}
	if cookie != "sessionid=s3cr3t-cookie-value" {
		t.Errorf("cookie sent = %q, want the session's cookie", cookie)
	}
	if authorization != "Bearer caller" {
		t.Errorf("authorization sent = %q, want the caller's to win", authorization)
	}
	if caller != "yes" {
		t.Errorf("caller header = %q, want it sent", caller)
	}
	if !strings.Contains(out, "status 200") {
		t.Errorf("output = %q, want the status", out)
	}
	if !strings.Contains(out, "receipt ") {
		t.Errorf("output = %q, want the receipt it landed in", out)
	}
	if id := receiptID(out); id != "" {
		os.Remove(filepath.Join(receipts, id))
	}
}

// The coordinator validates the arguments against the schema before the tool
// ever sees them, so a schema that refuses a legal call costs a retry.
func TestTheAuthSchemaAcceptsTheArguments(t *testing.T) {
	schema := (&AuthTool{}).Schema()
	for _, args := range []map[string]any{
		{
			"mode":    "login",
			"name":    "admin",
			"url":     "https://example.com/login",
			"method":  "POST",
			"body":    map[string]any{"user": "admin", "password": "hunter2"},
			"headers": map[string]any{"Content-Type": "application/json", "X-Count": 1},
			"timeout": 15,
		},
		{
			"mode":    "replay",
			"session": "admin",
			"url":     "https://example.com/admin",
			"body":    map[string]any{"id": 42},
		},
		{"mode": "list"},
		{"mode": "delete", "name": "admin"},
	} {
		if err := ValidateArguments(schema, args); err != nil {
			t.Errorf("the schema refused %v: %v", args, err)
		}
	}
	if err := ValidateArguments(schema, map[string]any{"mode": "burn"}); err == nil {
		t.Error("the schema accepted an unknown mode")
	}
}

// The schema requires nothing, because a schema cannot make a field
// conditionally required: the mode decides, and the tool body refuses the
// call that is short of an argument.
func TestTheAuthSchemaRequiresNothing(t *testing.T) {
	schema := (&AuthTool{}).Schema()
	required, _ := schema["required"].([]string)
	if len(required) != 0 {
		t.Errorf("required = %v, want none: the mode decides", required)
	}
	if err := ValidateArguments(schema, map[string]any{}); err != nil {
		t.Errorf("the schema refused a call with no arguments: %v", err)
	}
}

// The behavioural facts a schema cannot express have to survive the merge:
// the login and the replay are gated and receipted, and a cookie value does
// not leave the machine.
func TestTheAuthDescriptionKeepsTheBehaviourFacts(t *testing.T) {
	description := (&AuthTool{}).Description()
	for _, want := range []string{"scope-gated", "receipted", "never leave the machine"} {
		if !strings.Contains(description, want) {
			t.Errorf("description = %q, want it to keep %q", description, want)
		}
	}
}

// The three catalogue entries became one, so the model reads one schema
// instead of three.
func TestTheRegistryOffersOneAuthTool(t *testing.T) {
	reg := DefaultRegistryForUser("x", nil)
	if _, ok := reg.Get("auth"); !ok {
		t.Error("the registry has no auth tool")
	}
	for _, name := range []string{"auth_login", "auth_replay", "auth_sessions"} {
		if _, ok := reg.Get(name); ok {
			t.Errorf("the registry still offers %q", name)
		}
	}
	count := 0
	for _, item := range reg.Offered() {
		if item.Name() == "auth" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the catalogue carries %d auth entries, want 1", count)
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

// reconReceiptsDir is where the recon package writes its receipts.
//
// The base is resolved when that package starts, so a test that points
// CERAA_HOME somewhere else afterwards does not move it, and the path is
// read from the environment as the process saw it.
func reconReceiptsDir() string {
	base := os.Getenv("CERAA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config", "ceraa")
	}
	return filepath.Join(base, "recon", "receipts")
}

// receiptID pulls the receipt id out of a tool answer.
func receiptID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "receipt ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "receipt "))
		}
	}
	return ""
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
