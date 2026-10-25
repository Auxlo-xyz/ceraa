package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ceraa/harness/inbox"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// A turn that opens with "I can't" used to reach the user before anything could
// be done about it. The opening is now held, the request is abandoned without
// emitting a byte, and the run re-asks once with a nudge to investigate. This
// pins all three: nothing leaked, the nudge reached the provider, and the
// replacement answer is the only text the renderer saw.
func TestCoordinatorNudgesOnRefusalBeforeStreamingIt(t *testing.T) {
	var requests atomic.Int32
	var secondCarriedNudge atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		n := requests.Add(1)
		if n == 2 {
			raw, _ := json.Marshal(body["messages"])
			secondCarriedNudge.Store(strings.Contains(string(raw), "web search"))
		}
		var payload map[string]any
		if n == 1 {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "I can't do that."}}}}
		} else {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "Found it: search the docs, then run the command."}}}}
		}
		encoded, _ := json.Marshal(payload)
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

	var streamed strings.Builder
	done := make(chan string, 1)
	err = RunAsync(context.Background(), Config{SessionID: "refusal-nudge", SystemPrompt: "test", ModelCatalogURL: "http://127.0.0.1:1"}, store, tool.DefaultRegistry(), operation.New(nil), ib, Callbacks{
		OnContent: func(text string) { streamed.WriteString(text) },
		OnDone:    func(answer string) { done <- answer },
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case answer := <-done:
		if answer != "Found it: search the docs, then run the command." {
			t.Fatalf("answer = %q", answer)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the run did not finish")
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want one abandoned attempt and one nudge", requests.Load())
	}
	if !secondCarriedNudge.Load() {
		t.Fatal("the retry did not carry the nudge")
	}
	if got := streamed.String(); strings.Contains(strings.ToLower(got), "can't") {
		t.Fatalf("the refusal reached the renderer: %q", got)
	}
	if got := streamed.String(); !strings.Contains(got, "Found it") {
		t.Fatalf("streamed text = %q", got)
	}
}
