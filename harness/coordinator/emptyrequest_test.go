package coordinator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// A window too small for the system prompt and one turn used to produce a
// request with no user turn at all, and the gateway answered it with
// "invalid params, messages must not be empty (2013)" after the run had already
// been recorded as a provider failure. The request was wrong before it left, so
// the run has to fail here, with a reason the owner can act on, and the provider
// must not be called at all.
func TestRunFailsBeforeSendingARequestWithNoTurn(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := New(RunnerConfig{
		SessionID: "empty-request",
		// Far larger than the window below, so the trim can keep nothing but the
		// recorded prompt and the request would have gone out with no turn.
		SystemPrompt:    strings.Repeat("you are a test agent. ", 2000),
		Model:           llm.FromProvider(server.URL, "key", "test-model"),
		ContextLimit:    2000,
		WorkDir:         dir,
		GoalDir:         dir,
		ToolOutputDir:   dir,
		ModelCatalogURL: "http://127.0.0.1:1",
	}, store, tool.NewRegistry())

	_, runErr := runner.Run(context.Background(), "hello")
	if runErr == nil {
		t.Fatal("the run completed with a request that carried no turn")
	}
	if !strings.Contains(runErr.Error(), "context window exhausted") {
		t.Fatalf("error = %q, want a local context report", runErr)
	}
	if requests.Load() != 0 {
		t.Fatalf("the provider was called %d times; an empty request must not be sent", requests.Load())
	}

	sess, err := store.Load("empty-request")
	if err != nil {
		t.Fatal(err)
	}
	last := sess.Runs[len(sess.Runs)-1]
	if last.Status != sessionstore.RunFailed {
		t.Fatalf("status = %q, want %q", last.Status, sessionstore.RunFailed)
	}
	// The failure text is shown in a chat and read later: it must be one short
	// line, not a pasted request or a wrapper chain.
	if runes := []rune(last.Error); len(runes) > 300 {
		t.Fatalf("recorded error is %d runes: %q", len(runes), last.Error)
	}
}
