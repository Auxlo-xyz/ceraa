package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"ceraa/harness/sessionstore"
	"ceraa/internal/config"
)

// stubProvider answers with a scripted sequence of turns.
//
// A live run is the only way to prove the seam works, and a real provider is not
// available to a test. The stub speaks the same SSE shape the client already
// parses, so a change to the request the renderer depends on shows up here.
func stubProvider(t *testing.T, turns func(call int32) []string) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		// prompt_tokens has to describe the request it answered. A fixed 100 next
		// to a prompt we measured at 2727 is a provider reporting a running total,
		// which reconciliation rejects, and a rejected run has no input figure to
		// put in the footer. Estimating off the request body keeps the stub inside
		// the band that says "this number is about the thing you sent".
		prompt := len(body) / 4
		if prompt == 0 {
			prompt = 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range turns(n) {
			encoded, _ := json.Marshal(map[string]any{
				"choices": []any{map[string]any{"delta": map[string]any{"content": chunk}}},
				"usage": map[string]any{
					"prompt_tokens": prompt, "completion_tokens": 20, "total_tokens": prompt + 20,
				},
			})
			fmt.Fprintf(w, "data: %s\n\n", encoded)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTestSession builds a session over a stub provider with everything scoped to
// a temp dir, so a test cannot read or write a real configuration.
func newTestSession(t *testing.T, srv *httptest.Server) *Session {
	t.Helper()
	dir := t.TempDir()
	// The secret store is process-global, so a test must not depend on a real one
	// having been opened, and must not overwrite the user's.
	t.Setenv("CERAA_DATA_DIR", filepath.Join(dir, "data"))
	cfg := &config.Config{
		SessionFile:    filepath.Join(dir, "sessions"),
		MemoryFile:     filepath.Join(dir, "memory"),
		SkillFile:      filepath.Join(dir, "skills"),
		EvaluationFile: filepath.Join(dir, "evals.json"),
		ToolOutputDir:  filepath.Join(dir, "toolout"),
		LLM: config.LLMProvider{
			URL:    srv.URL,
			Model:  "stub/model",
			APIKey: "test",
		},
	}
	var out strings.Builder
	s, err := NewSession(SessionOptions{
		Config:    cfg,
		Renderer:  NewRenderer(&out, Plain()),
		SessionID: "test-session",
		// Deny every permission, so a test can never block on a prompt.
		ReadLine: func(string) (string, error) { return "n", nil },
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return s
}

func TestSessionRunProducesVisibleOutput(t *testing.T) {
	srv := stubProvider(t, func(int32) []string { return []string{"hello ", "from ", "the model"} })
	s := newTestSession(t, srv)
	if err := s.RunContext(context.Background(), "hi"); err != nil {
		t.Fatalf("RunContext: %v", err)
	}
	// The renderer writes into the builder the test owns through the session, so
	// read it back off the buffer the session was given.
	got := sessionOutput(t, s)
	if !strings.Contains(got, "hello from the model") {
		t.Errorf("answer not rendered:\n%s", got)
	}
}

func TestSessionCarriesContextAcrossTurns(t *testing.T) {
	// This is the whole difference from the one-shot path: a session rebuilt per
	// turn is a session that forgets.
	var prompts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		reply := "ok"
		if prompts.Add(1) > 1 {
			// The second request must carry the first exchange, or the REPL is a
			// series of unrelated prompts wearing a loop.
			raw, _ := json.Marshal(body)
			if !strings.Contains(string(raw), "first question") {
				t.Errorf("second request did not carry the first question")
			}
			if !strings.Contains(string(raw), "first answer") {
				t.Errorf("second request did not carry the first answer")
			}
			reply = "second answer"
		} else {
			reply = "first answer"
		}
		encoded, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": reply}}},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", encoded)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	s := newTestSession(t, srv)
	if err := s.RunContext(context.Background(), "first question"); err != nil {
		t.Fatal(err)
	}
	if err := s.RunContext(context.Background(), "second question"); err != nil {
		t.Fatal(err)
	}
	if got := prompts.Load(); got != 2 {
		t.Errorf("provider saw %d requests, want 2", got)
	}
}

func TestSessionFooterReportsUsage(t *testing.T) {
	srv := stubProvider(t, func(int32) []string { return []string{"ok"} })
	s := newTestSession(t, srv)
	if err := s.RunContext(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	got := sessionOutput(t, s)
	if !regexp.MustCompile(`\bin \d+`).MatchString(got) {
		t.Errorf("footer did not report input tokens:\n%s", got)
	}
}

func TestSessionStoresTheTurn(t *testing.T) {
	// A run that is not in the store is a run that cannot be resumed, which is the
	// one feature the REPL needs that the one-shot path did not have.
	srv := stubProvider(t, func(int32) []string { return []string{"remembered"} })
	s := newTestSession(t, srv)
	if err := s.RunContext(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Store().Load("test-session")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(sess.Messages) == 0 {
		t.Fatal("no messages were stored")
	}
	var sawAnswer bool
	for _, m := range sess.Messages {
		if strings.Contains(m.Content, "remembered") {
			sawAnswer = true
		}
	}
	if !sawAnswer {
		t.Error("the answer was not written to the session")
	}
}

func TestSessionIsResumableByID(t *testing.T) {
	srv := stubProvider(t, func(int32) []string { return []string{"ok"} })
	dir := t.TempDir()
	t.Setenv("CERAA_DATA_DIR", filepath.Join(dir, "data"))
	mk := func() *Session {
		cfg := &config.Config{
			SessionFile: filepath.Join(dir, "sessions"),
			MemoryFile:  filepath.Join(dir, "memory"),
			SkillFile:   filepath.Join(dir, "skills"),
			// Every store path is absolute and inside the temp dir. A relative one
			// resolves against the package directory, so a test that omits it writes
			// evaluations.json into the source tree.
			EvaluationFile: filepath.Join(dir, "evals.json"),
			ToolOutputDir:  filepath.Join(dir, "toolout"),
			LLM:            config.LLMProvider{URL: srv.URL, Model: "stub/model", APIKey: "test"},
		}
		var out strings.Builder
		s, err := NewSession(SessionOptions{
			Config: cfg, Renderer: NewRenderer(&out, Plain()),
			SessionID: "resume-me", ReadLine: func(string) (string, error) { return "n", nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := mk()
	if err := first.RunContext(context.Background(), "before the restart"); err != nil {
		t.Fatal(err)
	}
	second := mk()
	if err := second.RunContext(context.Background(), "after the restart"); err != nil {
		t.Fatal(err)
	}
	sess, err := second.Store().Load("resume-me")
	if err != nil {
		t.Fatal(err)
	}
	var sawBefore bool
	for _, m := range sess.Messages {
		if strings.Contains(m.Content, "before the restart") {
			sawBefore = true
		}
	}
	if !sawBefore {
		t.Error("a session reopened by id lost the earlier turn")
	}
}

func TestSessionDeniesAPermissionPromptByDefault(t *testing.T) {
	// The readLine hook answers "n", so a test that reaches a prompt must not hang.
	// If this returns, the deny path works.
	srv := stubProvider(t, func(int32) []string { return []string{"ok"} })
	s := newTestSession(t, srv)
	denied, err := s.authorizeTool(context.Background(), "call-1", "write", map[string]any{"path": "x"})
	if err != nil {
		t.Fatalf("authorizeTool: %v", err)
	}
	if denied {
		t.Error("a denied prompt returned allowed")
	}
}

func TestSessionAllowsOnce(t *testing.T) {
	srv := stubProvider(t, func(int32) []string { return []string{"ok"} })
	s := newTestSession(t, srv)
	s.readLine = func(string) (string, error) { return "y", nil }
	allowed, err := s.authorizeTool(context.Background(), "call-2", "write", map[string]any{"path": "x"})
	if err != nil {
		t.Fatalf("authorizeTool: %v", err)
	}
	if !allowed {
		t.Error("'y' did not allow once")
	}
}

func TestSessionRemembersAllowAlways(t *testing.T) {
	srv := stubProvider(t, func(int32) []string { return []string{"ok"} })
	s := newTestSession(t, srv)
	s.readLine = func(string) (string, error) { return "a", nil }
	if _, err := s.authorizeTool(context.Background(), "c1", "bash", map[string]any{"command": "ls"}); err != nil {
		t.Fatal(err)
	}
	// A second ask with no input at all must be allowed by the remembered decision.
	s.readLine = func(string) (string, error) { return "", fmt.Errorf("no input") }
	allowed, err := s.authorizeTool(context.Background(), "c2", "bash", map[string]any{"command": "ls"})
	if err != nil {
		t.Fatalf("authorizeTool: %v", err)
	}
	if !allowed {
		t.Error("'always' did not stick for the session")
	}
}

func TestSessionNeedsAuthorizationCoversWritesAndRiskyCommands(t *testing.T) {
	srv := stubProvider(t, func(int32) []string { return []string{"ok"} })
	s := newTestSession(t, srv)
	for _, name := range []string{"write", "edit_line", "batch_edit_file", "delete", "move", "copy"} {
		if !s.needsAuthorization(name, map[string]any{"path": "x"}) {
			t.Errorf("%s should ask", name)
		}
	}
	for _, cmd := range []string{"rm -rf /tmp/x", "sudo ls", "curl http://x", "git push"} {
		if !s.needsAuthorization("bash", map[string]any{"command": cmd}) {
			t.Errorf("bash %q should ask", cmd)
		}
	}
	if s.needsAuthorization("bash", map[string]any{"command": "go test ./..."}) {
		t.Error("an ordinary command asked for permission")
	}
	if s.needsAuthorization("read", map[string]any{"path": "x"}) {
		t.Error("read asked for permission")
	}
}

// sessionOutput reads back what the session's renderer wrote.
func sessionOutput(t *testing.T, s *Session) string {
	t.Helper()
	b, ok := s.render.out.(*strings.Builder)
	if !ok {
		t.Fatal("test session renderer is not writing to a builder")
	}
	return b.String()
}

var _ = sessionstore.Session{}
