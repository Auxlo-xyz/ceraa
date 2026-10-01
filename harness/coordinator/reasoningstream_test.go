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
