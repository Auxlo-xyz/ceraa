package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ceraa/harness/compaction"
	"ceraa/harness/inbox"
	"ceraa/harness/llm"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// Drives a whole run against a stubbed provider and a stubbed model catalog.
//
// The unit tests above cover the resolver and the recalibration function. Neither
// proves the run path calls it, and that wiring is exactly the part that was
// wrong: the catalog lookup already existed, already read ContextWindow, and
// already wrote it to the run record without it ever reaching the budget. A test
// of the function alone would have passed throughout.
func TestRunBudgetsFromTheCatalogRatherThanAConstant(t *testing.T) {
	const advertised = 32000
	var providerRequests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		encoded, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}}},
		})
		fmt.Fprintf(w, "data: %s\n\n", encoded)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()

	var catalogRequests atomic.Int32
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		catalogRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":[{"id":"stub/small","name":"stub small","context_length":%d,"max_completion_tokens":4096}]}`, advertised)
	}))
	defer catalog.Close()

	t.Setenv("CERAA_API_URL", provider.URL)
	t.Setenv("CERAA_API_KEY", "test")
	t.Setenv("CERAA_MODEL", "stub/small")

	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Stop()

	compactor := compaction.New(llm.ProviderConfig{})
	done := make(chan string, 1)
	err = RunAsync(context.Background(), Config{
		SessionID:       "catalog-budget",
		SystemPrompt:    "test",
		ModelCatalogURL: catalog.URL,
		Compactor:       compactor,
	}, store, tool.DefaultRegistry(), operation.New(nil), ib, Callbacks{
		OnDone: func(answer string) { done <- answer },
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case answer := <-done:
		if answer != "ok" {
			t.Fatalf("answer = %q", answer)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not complete")
	}

	if catalogRequests.Load() == 0 {
		t.Fatal("the catalog was never consulted, so nothing could have recalibrated")
	}
	// The advertised window, unchanged. The headroom is the compactor's business
	// and is applied once, where it is needed.
	if got := compactor.ContextLimit; got != advertised {
		t.Errorf("compactor budget = %d, want %d, the advertised window", got, advertised)
	}
	// And emphatically not the old constant.
	if compactor.ContextLimit == 236000 {
		t.Error("the run budgeted against the old hardcoded constant")
	}
}

// A catalog that cannot answer must not leave the budget at zero or at a
// remembered constant; the conservative fallback has to take over.
func TestRunFallsBackWhenTheCatalogIsUnreachable(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		encoded, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}}},
		})
		fmt.Fprintf(w, "data: %s\n\n", encoded)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()
	t.Setenv("CERAA_API_URL", provider.URL)
	t.Setenv("CERAA_API_KEY", "test")
	t.Setenv("CERAA_MODEL", "stub/unknown")

	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Stop()
	compactor := compaction.New(llm.ProviderConfig{})
	done := make(chan string, 1)
	err = RunAsync(context.Background(), Config{
		SessionID:       "catalog-unreachable",
		SystemPrompt:    "test",
		ModelCatalogURL: "http://127.0.0.1:1",
		Compactor:       compactor,
	}, store, tool.DefaultRegistry(), operation.New(nil), ib, Callbacks{
		OnDone: func(answer string) { done <- answer },
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not complete with an unreachable catalog")
	}
	if compactor.ContextLimit != llm.FallbackContextWindow {
		t.Errorf("budget = %d, want the fallback %d", compactor.ContextLimit, llm.FallbackContextWindow)
	}
}
