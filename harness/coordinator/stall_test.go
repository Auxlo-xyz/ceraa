package coordinator

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

func TestStallWatchCancelsAfterSilence(t *testing.T) {
	watch := newStallWatch(80 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch.Start(cancel)
	defer watch.Stop()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the watchdog did not cancel a silent request")
	}
	if watch.Err() == nil {
		t.Fatal("a canceled request is not reported as a stall")
	}
}

func TestStallWatchKickExtendsTheWindow(t *testing.T) {
	watch := newStallWatch(120 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch.Start(cancel)
	defer watch.Stop()

	deadline := time.After(480 * time.Millisecond)
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			if ctx.Err() != nil {
				t.Fatal("a stream that kept producing output was cancelled")
			}
			return
		case <-ticker.C:
			watch.Kick()
		}
	}
}

func TestOnlyUsableDeltasCountAsProgress(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delta llm.ProviderResponse
		want  bool
	}{
		{"empty", llm.ProviderResponse{}, false},
		{"usage only", llm.ProviderResponse{Usage: &llm.UsageInfo{TotalTokens: 5}}, false},
		{"content", llm.ProviderResponse{Content: "ok"}, true},
		{"reasoning", llm.ProviderResponse{Reasoning: "thinking"}, true},
		{"tool call", llm.ProviderResponse{ToolCalls: []llm.StreamedToolCall{{ID: "1", Name: "read"}}}, true},
		{"reasoning details", llm.ProviderResponse{ReasoningDetails: []byte(`[{"type":"reasoning.text","text":"x"}]`)}, true},
	} {
		if got := countsAsProgress(tc.delta); got != tc.want {
			t.Errorf("%s: countsAsProgress = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A gateway that holds the connection open with keepalives defeats the
// transport's byte-idle watchdog by design: every byte resets it. The run then
// waits with nothing to show, which the owner experiences as a bot that went
// quiet. The run has to fail with a reason instead of waiting forever.
func TestRunFailsWhenTheProviderOnlySendsKeepalives(t *testing.T) {
	previous := stallLimit
	stallLimit = 250 * time.Millisecond
	t.Cleanup(func() { stallLimit = previous })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				// A comment line is a well-formed SSE keepalive and carries no
				// event, which is exactly the shape that used to wait forever.
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := New(RunnerConfig{
		SessionID:       "stalled",
		SystemPrompt:    "you are a test agent",
		Model:           llm.FromProvider(server.URL, "key", "test-model"),
		ContextLimit:    32000,
		WorkDir:         dir,
		GoalDir:         dir,
		ToolOutputDir:   dir,
		ModelCatalogURL: "http://127.0.0.1:1",
	}, store, tool.NewRegistry())

	started := time.Now()
	_, runErr := runner.Run(context.Background(), "say hello")
	if runErr == nil {
		t.Fatal("a keepalive-only provider completed the run instead of stalling")
	}
	if !strings.Contains(runErr.Error(), "provider stalled") {
		t.Fatalf("error = %q, want a stall report", runErr)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the stall took %s; the watchdog did not bound the wait", elapsed)
	}

	sess, err := store.Load("stalled")
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Runs) == 0 {
		t.Fatal("the run was not recorded")
	}
	last := sess.Runs[len(sess.Runs)-1]
	if last.Status != sessionstore.RunFailed {
		t.Fatalf("run status = %q, want %q: a stall is a failure, not a cancellation", last.Status, sessionstore.RunFailed)
	}
	if !strings.Contains(last.Error, "provider stalled") {
		t.Fatalf("recorded error = %q, want the stall reason", last.Error)
	}
}

// The watchdog must not turn a slow but healthy answer into a failure. This is
// the other half of the contract, and the half a too-short window breaks.
func TestRunAcceptsAnAnswerThatKeepsArriving(t *testing.T) {
	previous := stallLimit
	stallLimit = 400 * time.Millisecond
	t.Cleanup(func() { stallLimit = previous })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, word := range []string{"slow ", "but ", "alive"} {
			payload := fmt.Sprintf(`data: {"choices":[{"delta":{"content":%q}}]}`, word)
			if _, err := fmt.Fprint(w, payload+"\n\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := New(RunnerConfig{
		SessionID:       "alive",
		SystemPrompt:    "you are a test agent",
		Model:           llm.FromProvider(server.URL, "key", "test-model"),
		ContextLimit:    32000,
		WorkDir:         dir,
		GoalDir:         dir,
		ToolOutputDir:   dir,
		ModelCatalogURL: "http://127.0.0.1:1",
	}, store, tool.NewRegistry())

	result, runErr := runner.Run(context.Background(), "say hello")
	if runErr != nil {
		t.Fatalf("a streaming answer was failed: %v", runErr)
	}
	if result.Final != "slow but alive" {
		t.Fatalf("answer = %q, want %q", result.Final, "slow but alive")
	}
}
