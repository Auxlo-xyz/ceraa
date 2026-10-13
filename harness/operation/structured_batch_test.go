package operation

import (
	"context"
	"sync"
	"testing"
	"time"
)

type structuredBatchRunner struct {
	mu      sync.Mutex
	started map[string]time.Time
	peak    int
	active  int
}

func (r *structuredBatchRunner) RunTool(ctx context.Context, spec Spec) (string, error) {
	name, _ := spec.Args["name"].(string)
	r.mu.Lock()
	if r.started == nil {
		r.started = make(map[string]time.Time)
	}
	r.started[name] = time.Now()
	r.active++
	if r.active > r.peak {
		r.peak = r.active
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.active--
		r.mu.Unlock()
	}()
	select {
	case <-time.After(30 * time.Millisecond):
		return name, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (r *structuredBatchRunner) snapshot() (int, map[string]time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	started := make(map[string]time.Time, len(r.started))
	for name, when := range r.started {
		started[name] = when
	}
	return r.peak, started
}

func TestRunStructuredBatchBoundsParallelismAndPreservesOrder(t *testing.T) {
	runner := &structuredBatchRunner{}
	items := []StructuredBatchItem{
		{ID: "one", Spec: Spec{Type: "test", Args: map[string]any{"name": "one"}}},
		{ID: "two", Spec: Spec{Type: "test", Args: map[string]any{"name": "two"}}},
		{ID: "three", Spec: Spec{Type: "test", Args: map[string]any{"name": "three"}}},
	}
	results, err := RunStructuredBatch(context.Background(), runner, items, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"one", "two", "three"} {
		if results[i].OpID != items[i].ID || results[i].Output != want || results[i].Status != StatusCompleted {
			t.Fatalf("result[%d] = %#v, want completed %q", i, results[i], want)
		}
	}
	peak, started := runner.snapshot()
	if peak > 2 {
		t.Fatalf("peak concurrency = %d, want at most 2", peak)
	}
	if !started["one"].Before(started["three"]) {
		t.Fatalf("third item started before an initial slot completed: %#v", started)
	}
}

func TestRunStructuredBatchRejectsDuplicateIDsBeforeExecution(t *testing.T) {
	runner := &structuredBatchRunner{}
	_, err := RunStructuredBatch(context.Background(), runner, []StructuredBatchItem{
		{ID: "same", Spec: Spec{Type: "test"}},
		{ID: "same", Spec: Spec{Type: "test"}},
	}, 2)
	if err == nil {
		t.Fatal("duplicate IDs were accepted")
	}
	if _, started := runner.snapshot(); len(started) != 0 {
		t.Fatalf("runner started despite invalid batch: %#v", started)
	}
}
