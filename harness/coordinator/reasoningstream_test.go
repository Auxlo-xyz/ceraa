package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ceraa/harness/inbox"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// Streamed reasoning arrived at the callback as the whole accumulated string
// rather than the newly arrived part, while content arrived correctly as a
// delta. Every consumer appends what it is handed, so each reasoning delta
// reprinted everything seen so far: a single short thought about a greeting
// filled the screen with that same sentence, each repetition one delta longer
// than the last, with the previous repetition's tail welded onto the next one's
// head.
//
// This asserts the contract from both sides: the concatenation is the whole
// thought exactly once, and no individual callback received more than it should.
func TestStreamedReasoningReachesTheCallbackOnceAndInOrder(t *testing.T) {
	deltas := []string{
		"The user said ",
		`"hello". This is a `,
		"simple greeting.",
		" Greet back.",
	}

	var (
		mu       sync.Mutex
		received []string
		joined   strings.Builder
	)
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, s)
		joined.WriteString(s)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range deltas {
			payload := map[string]any{
				"choices": []any{map[string]any{
					"delta": map[string]any{"reasoning_content": d},
				}},
			}
			encoded, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		}
		// The turn needs a final answer or the runner asks for one again.
		final := map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": "hello"}}},
		}
		encoded, _ := json.Marshal(final)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("CERAA_API_URL", server.URL)
	t.Setenv("CERAA_API_KEY", "test")
	t.Setenv("CERAA_MODEL", "test")

	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Stop()

	done := make(chan string, 1)
	err = RunAsync(context.Background(),
		Config{SessionID: "reasoning-delta", SystemPrompt: "test", ModelCatalogURL: "http://127.0.0.1:1"},
		store, tool.DefaultRegistry(), operation.New(nil), ib,
		Callbacks{OnReasoning: record, OnDone: func(answer string) { done <- answer }})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-context.Background().Done():
	}

	want := strings.Join(deltas, "")
	if got := joined.String(); got != want {
		t.Fatalf("the callback was handed more than the thought.\n got %q\nwant %q", got, want)
	}
	// A cumulative callback would have delivered a prefix that keeps growing.
	// Every chunk must be a piece of the thought and must not repeat one.
	for i, chunk := range received {
		if strings.Count(strings.Join(received[i:], ""), chunk) > 1 && len(chunk) > 1 {
			t.Errorf("chunk %d repeats itself: %q", i, chunk)
		}
	}
}

// OnStepReasoningOnly means "this step was working, not answering", and a step
// with no tool calls is the answer by this runner's own definition.
//
// Firing it unconditionally fired it on the final step too, where the content
// channel was carrying the answer rather than reasoning. A renderer that trusts
// the signal folds the answer into a thought and releases the message it was
// streaming in, so the final publish has nothing to edit and sends the same text
// again as a second message. It only reproduced when the last step also carried
// tool calls, which is what made it look intermittent.
func TestStepReasoningOnlyNeverFiresOnTheFinalStep(t *testing.T) {
	var (
		mu       sync.Mutex
		steps    int
		firedFor int
		requests int
	)

	// The first request is a step that ends in a tool call, with its reasoning on
	// the content channel: the shape Atria produces when no reasoning level is
	// asked for. Every request after that is the tool-result turn, which ends in
	// the final answer and has no tool calls.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(delta map[string]any) {
			payload := map[string]any{
				"choices": []any{map[string]any{"delta": delta}},
			}
			encoded, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		}
		mu.Lock()
		requests++
		first := requests == 1
		mu.Unlock()
		if first {
			send(map[string]any{"content": "Let me look at the file. "})
			send(map[string]any{"tool_calls": []any{map[string]any{
				"index": 0,
				"id":    "call-1",
				"type":  "function",
				"function": map[string]any{
					"name":      "read",
					"arguments": `{"path":"apitrace.go"}`,
				},
			}}})
		} else {
			send(map[string]any{"content": "It is clean."})
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("CERAA_API_URL", server.URL)
	t.Setenv("CERAA_API_KEY", "test")
	t.Setenv("CERAA_MODEL", "test")

	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Stop()

	done := make(chan string, 1)
	var finalAnswer string
	err = RunAsync(context.Background(),
		Config{SessionID: "step-boundary", SystemPrompt: "test", ModelCatalogURL: "http://127.0.0.1:1"},
		store, tool.DefaultRegistry(), operation.New(nil), ib,
		Callbacks{
			OnStepReasoningOnly: func() {
				mu.Lock()
				steps++
				firedFor = steps
				mu.Unlock()
			},
			OnDone: func(answer string) {
				mu.Lock()
				finalAnswer = answer
				mu.Unlock()
				done <- answer
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the run never finished")
	}

	mu.Lock()
	fired := firedFor
	answer := finalAnswer
	mu.Unlock()
	if answer != "It is clean." {
		t.Fatalf("final answer = %q", answer)
	}
	// One fire, on the step that had tool calls. The final step is the answer and
	// must never be reported as reasoning-only, or the renderer treats the answer
	// as reasoning and duplicates it.
	if fired != 1 {
		t.Errorf("OnStepReasoningOnly fired %d time(s) (last at step %d); it must fire exactly once, only for the step that ended in tool calls", fired, fired)
	}
}
