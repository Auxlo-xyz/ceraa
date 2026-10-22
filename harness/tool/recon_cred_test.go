package tool

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/creds"
	"ceraa/harness/jar"
	"ceraa/harness/operation"
	"ceraa/harness/recon"
)

// The cred tool is a credential test wearing a tool face: it sprays a list of
// credentials, it lists the candidate usernames and it counts the candidate
// passwords. The spray is exercised against a test server, so the suite
// never reaches the public internet.

// TestCredName pins the registry name, which is also the operation type.
func TestCredName(t *testing.T) {
	tool := &CredTool{}
	if tool.Name() != "cred" {
		t.Errorf("Name = %q, want cred", tool.Name())
	}
	spec, err := tool.Translate(map[string]any{"mode": "usernames", "domain": "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Type != tool.Name() {
		t.Errorf("spec.Type = %q and Name = %q; the runner resolves the executor by the type", spec.Type, tool.Name())
	}
}

// TestCredDescriptionNamesTheContract: the model reads this, so it has to
// say what the tool does not do.
func TestCredDescriptionNamesTheContract(t *testing.T) {
	description := (&CredTool{}).Description()
	for _, want := range []string{"scope-gated", "passwords never appear", "session"} {
		if !strings.Contains(description, want) {
			t.Errorf("the description does not mention %q: %s", want, description)
		}
	}
}

// TestCredTranslateRejectsAnImpossibleCall: without a url there is no
// spray, and an unknown mode is not a mode.
func TestCredTranslateRejectsAnImpossibleCall(t *testing.T) {
	tool := &CredTool{}
	for _, args := range []map[string]any{
		{"mode": "spray"},
		{"mode": "spray", "url": ""},
		{"mode": "spray", "url": "not a url"},
		{"mode": "spray", "url": "ftp://example.com/login"},
		{"mode": "spray", "url": "/login"},
		{"mode": "burn", "url": "https://example.com/login"},
		{"mode": 42},
	} {
		if spec, err := tool.Translate(args); err == nil {
			t.Errorf("Translate(%v) accepted, spec = %v", args, spec)
		}
	}
}

// TestCredTranslateAcceptsTheUsernamesAndPasswordsModes: neither of them
// needs a url, because neither of them sends anything.
func TestCredTranslateAcceptsTheUsernamesAndPasswordsModes(t *testing.T) {
	tool := &CredTool{}
	for _, mode := range []string{"usernames", "passwords"} {
		spec, err := tool.Translate(map[string]any{"mode": mode, "domain": "example.com"})
		if err != nil {
			t.Fatalf("Translate(%s): %v", mode, err)
		}
		if spec.Args["mode"] != mode {
			t.Errorf("mode = %v, want %s", spec.Args["mode"], mode)
		}
	}
}

// TestCredSchemaCoversTheArguments: a property the tool reads but the
// schema does not declare is refused by the harness before the tool runs.
func TestCredSchemaCoversTheArguments(t *testing.T) {
	tool := &CredTool{}
	schema := tool.Schema()
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range []string{
		"mode", "url", "method", "body", "headers", "username_field",
		"password_field", "success_marker", "success_field", "failure_marker",
		"locked_marker", "domain", "users", "passwords", "combo_file",
		"max_per_user", "delay_ms", "concurrency", "timeout", "session_name",
	} {
		if _, ok := properties[name]; !ok {
			t.Errorf("the schema does not declare %q", name)
		}
	}
	args := map[string]any{
		"mode":           "spray",
		"url":            "https://example.com/login",
		"method":         "POST",
		"body":           "username={USERNAME}&password={PASSWORD}",
		"headers":        map[string]any{"X-Count": 1},
		"username_field": "username",
		"password_field": "password",
		"success_marker": "welcome back",
		"success_field":  "status",
		"failure_marker": []any{"invalid password"},
		"locked_marker":  []any{"account locked"},
		"domain":         "example.com",
		"users":          []any{"admin"},
		"passwords":      []any{"hunter2"},
		"combo_file":     "combos.txt",
		"max_per_user":   float64(3),
		"delay_ms":       float64(100),
		"concurrency":    float64(1),
		"timeout":        float64(15),
		"session_name":   "spray",
	}
	if err := ValidateArguments(schema, args); err != nil {
		t.Errorf("the harness refuses the declared arguments: %v", err)
	}
	if err := ValidateArguments(schema, map[string]any{"mode": "burn"}); err == nil {
		t.Error("the schema accepted an unknown mode")
	}
}

// TestCredTranslateReadsEveryArgument: the canonical argument map carries
// what the caller asked for.
func TestCredTranslateReadsEveryArgument(t *testing.T) {
	tool := &CredTool{}
	spec, err := tool.Translate(map[string]any{
		"mode":           "spray",
		"url":            "https://example.com/login",
		"method":         "post",
		"body":           "username={USERNAME}&password={PASSWORD}",
		"headers":        map[string]any{"X-Count": 1},
		"username_field": "email",
		"password_field": "pass",
		"success_marker": "welcome back",
		"failure_marker": []any{"invalid password"},
		"locked_marker":  []any{"account locked"},
		"users":          []any{"admin"},
		"passwords":      []any{"hunter2"},
		"max_per_user":   float64(5),
		"delay_ms":       float64(250),
		"concurrency":    float64(2),
		"timeout":        float64(30),
		"session_name":   "spray",
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if method, _ := spec.Args["method"].(string); method != "POST" {
		t.Errorf("method = %v, want POST", spec.Args["method"])
	}
	if field, _ := spec.Args["username_field"].(string); field != "email" {
		t.Errorf("username_field = %v", spec.Args["username_field"])
	}
	if perUser, _ := spec.Args["max_per_user"].(int); perUser != 5 {
		t.Errorf("max_per_user = %v, want 5", spec.Args["max_per_user"])
	}
	if timeout, _ := spec.Args["timeout"].(int); timeout != 30 {
		t.Errorf("timeout = %v, want 30", spec.Args["timeout"])
	}
	if users, _ := spec.Args["users"].([]string); len(users) != 1 || users[0] != "admin" {
		t.Errorf("users = %v", spec.Args["users"])
	}
	if headers, _ := spec.Args["headers"].(map[string]string); headers["X-Count"] != "1" {
		t.Errorf("headers = %v, want the coerced string", spec.Args["headers"])
	}
}

// TestCredTranslateResultPassesOutput: result translation is a pass
// through of the recorded output.
func TestCredTranslateResultPassesOutput(t *testing.T) {
	tool := &CredTool{}
	op := operation.Operation{Output: "cred: spray https://example.com/login"}
	if got := tool.TranslateResult(op); got != op.Output {
		t.Errorf("TranslateResult = %q, want passthrough", got)
	}
}

// TestTheUsernamesModeNamesTheSources: a list with no provenance is a list
// nobody can reason about.
func TestTheUsernamesModeNamesTheSources(t *testing.T) {
	tool := &CredTool{}
	spec, err := tool.Translate(map[string]any{"mode": "usernames", "domain": "example.com"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}
	for _, want := range []string{"admin", "admin@example.com", "example.com", "(role)", "(role at domain)", "(domain)"} {
		if !strings.Contains(out, want) {
			t.Errorf("the username list does not carry %q:\n%s", want, out)
		}
	}
}

// TestThePasswordsModePrintsTheCountOnly: a password list in a chat
// transcript is a liability.
func TestThePasswordsModePrintsTheCountOnly(t *testing.T) {
	tool := &CredTool{}
	spec, err := tool.Translate(map[string]any{"mode": "passwords", "domain": "example.com"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}
	if !strings.Contains(out, "candidate passwords") {
		t.Errorf("the output does not report the count:\n%s", out)
	}
	for _, leak := range []string{"qwerty", "letmein", "password123"} {
		if strings.Contains(out, leak) {
			t.Errorf("the output leaked the password list:\n%s", out)
		}
	}
}

// TestTheSprayReportsTheOutcomesAndStoresTheSession: the end to end path.
// The password never reaches the output, and the session the login produced
// is on disk under its name.
func TestTheSprayReportsTheOutcomesAndStoresTheSession(t *testing.T) {
	const password = "correct-horse"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), password) {
			w.Header().Set("Set-Cookie", "sessionid=s3cr3t-cookie; Path=/; HttpOnly")
			io.WriteString(w, "<html>Welcome back</html>")
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "<p>Invalid password</p>")
	}))
	defer srv.Close()

	dir := t.TempDir()
	withCredSessionDir(t, dir)
	allowCredLoopback(t, srv)

	tool := &CredTool{}
	spec, err := tool.Translate(map[string]any{
		"mode":           "spray",
		"url":            srv.URL + "/login",
		"body":           "username={USERNAME}&password={PASSWORD}",
		"success_marker": "welcome back",
		"failure_marker": []any{"invalid password"},
		"users":          []any{"admin"},
		"passwords":      []any{"wrong-password", password},
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}
	if !strings.Contains(out, "user admin -> 401 wrong password") {
		t.Errorf("the output does not carry the rejected attempt:\n%s", out)
	}
	if !strings.Contains(out, "user admin -> 200 authenticated") {
		t.Errorf("the output does not carry the successful attempt:\n%s", out)
	}
	if !strings.Contains(out, "summary:") {
		t.Errorf("the output does not carry the summary:\n%s", out)
	}
	if strings.Contains(out, password) {
		t.Errorf("the output leaked the password:\n%s", out)
	}
	if !strings.Contains(out, "session") {
		t.Errorf("the output does not name the session:\n%s", out)
	}

	store, err := jar.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sessions, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %v, want one", sessions)
	}
	if sessions[0].Name != "cred-admin" {
		t.Errorf("session = %q, want the name derived from the username", sessions[0].Name)
	}
	if len(sessions[0].Cookies) != 1 || sessions[0].Cookies[0].Name != "sessionid" {
		t.Errorf("the session holds no cookie: %+v", sessions[0])
	}
}

// TestTheSprayResolvesARelativeComboFileAgainstTheWorkdir: a relative path
// from the model means where its commands run.
func TestTheSprayResolvesARelativeComboFileAgainstTheWorkdir(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "from-the-file") {
			io.WriteString(w, "<html>Welcome back</html>")
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "<p>Invalid password</p>")
	}))
	defer srv.Close()

	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "combos.txt"), []byte("admin:from-the-file\n"), 0o600); err != nil {
		t.Fatalf("write the combo file: %v", err)
	}
	withCredSessionDir(t, t.TempDir())
	allowCredLoopback(t, srv)

	tool := &CredTool{}
	spec, err := tool.Translate(map[string]any{
		"mode":           "spray",
		"url":            srv.URL + "/login",
		"body":           "username={USERNAME}&password={PASSWORD}",
		"success_marker": "welcome back",
		"combo_file":     "combos.txt",
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	spec.Args["workdir"] = workdir
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}
	if !strings.Contains(out, "user admin -> 200 authenticated") {
		t.Errorf("the combo file was not used:\n%s", out)
	}
}

// TestTheSprayNamesTheComboFileItCannotFind: the error has to be
// actionable.
func TestTheSprayNamesTheComboFileItCannotFind(t *testing.T) {
	workdir := t.TempDir()
	tool := &CredTool{}
	spec, err := tool.Translate(map[string]any{
		"mode":       "spray",
		"url":        "https://example.com/login",
		"body":       "username={USERNAME}&password={PASSWORD}",
		"combo_file": "absent.txt",
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	spec.Args["workdir"] = workdir
	_, err = tool.RunContext(context.Background(), spec)
	if err == nil {
		t.Fatal("the spray accepted a combo file that is not there")
	}
	if !strings.Contains(err.Error(), filepath.Join(workdir, "absent.txt")) {
		t.Errorf("error = %q, want it to name the resolved path", err.Error())
	}
}

// TestTheSprayOutputIsBounded: a spray of five hundred credentials is five
// hundred lines, and a chat message that carries five hundred lines is a
// message nobody reads. The summary carries the counts.
func TestTheSprayOutputIsBounded(t *testing.T) {
	result := creds.Result{
		Outcomes: make([]creds.Outcome, 200),
		Summary: creds.Summary{
			Attempts: 200,
			Failures: map[string]int{creds.FailureWrongPassword: 200},
		},
	}
	for i := range result.Outcomes {
		result.Outcomes[i] = creds.Outcome{
			Credential: creds.Credential{Username: "admin"},
			Status:     401,
			Failure:    creds.FailureWrongPassword,
		}
	}
	out := formatCredSpray(result, "https://example.com/login")
	if got := strings.Count(out, "user admin ->"); got != credMaxOutcomeLines {
		t.Errorf("printed %d outcomes, want the bound %d", got, credMaxOutcomeLines)
	}
	if !strings.Contains(out, "more attempts not shown") {
		t.Errorf("the output does not say what it left out:\n%s", out)
	}
	if !strings.Contains(out, "summary:") {
		t.Errorf("the output does not carry the summary:\n%s", out)
	}
}

// withCredSessionDir points the tool at a scratch directory for the
// duration of a test, and puts the old one back afterwards.
func withCredSessionDir(t *testing.T, dir string) {
	t.Helper()
	previous := credSessionDir
	credSessionDir = dir
	t.Cleanup(func() { credSessionDir = previous })
}

// allowCredLoopback opens the two checks a test server needs: the scope
// record that authorizes the loopback address as an explicit internal
// target, and the netguard exception that lets the dial reach it.
func allowCredLoopback(t *testing.T, srv *httptest.Server) {
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
