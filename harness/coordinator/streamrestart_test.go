package coordinator

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// A connection torn out under an answer is resent, and the resent answer is the
// answer: what the failed attempt managed to stream is dropped instead of being
// shown in front of the replacement.
func TestADroppedConnectionMidAnswerDoesNotRepeatTheAnswer(t *testing.T) {
	var requests atomic.Int32
	var restarts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(requests.Add(1)) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"half an \",\"reasoning_content\":\"thinking out loud\"}}]}\n\n"))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			time.Sleep(50 * time.Millisecond)
			hijack, ok := w.(http.Hijacker)
			if !ok {
				t.Error("the test server cannot hijack, so no drop can be simulated")
				return
			}
			conn, _, err := hijack.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"whole answer\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := New(RunnerConfig{
		SessionID:       "dropped",
		SystemPrompt:    "you are a test agent",
		Model:           llm.FromProvider(server.URL, "key", "test-model"),
		ContextLimit:    32000,
		WorkDir:         dir,
		GoalDir:         dir,
		ToolOutputDir:   dir,
		ModelCatalogURL: "http://127.0.0.1:1",
		Callbacks: Callbacks{
			OnStreamRestart: func() { restarts.Add(1) },
		},
	}, store, tool.NewRegistry())

	result, runErr := runner.Run(context.Background(), "say hello")
	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2: one dropped and one resent", got)
	}
	if got := restarts.Load(); got != 1 {
		t.Errorf("restarts = %d, want 1: the caller was told to drop what it holds", got)
	}
	if result.Final != "whole answer" {
		t.Errorf("final = %q, want only the resent answer", result.Final)
	}

	sess, err := store.Load("dropped")
	if err != nil {
		t.Fatal(err)
	}
	var last = sess.Messages[len(sess.Messages)-1]
	if last.Content != "whole answer" {
		t.Errorf("stored answer = %q, want only the resent answer", last.Content)
	}
	if last.Reasoning != "" {
		t.Errorf("stored reasoning = %q, want the dropped attempt's reasoning discarded", last.Reasoning)
	}
}
