package operation

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Lane behaviour is the whole point of the change: two lanes run at once, one
// lane runs in arrival order.
type laneRunner struct {
	mu       sync.Mutex
	order    []string
	running  int
	peak     int
	held     chan struct{}
	seen     chan string
	release  chan struct{}
	inFlight int
}

func (r *laneRunner) RunTool(ctx context.Context, spec Spec) (string, error) {
	r.mu.Lock()
	r.order = append(r.order, spec.Args["name"].(string))
	r.inFlight++
	if r.inFlight > r.peak {
		r.peak = r.inFlight
	}
	r.mu.Unlock()
	select {
	case r.seen <- spec.Args["name"].(string):
	case <-ctx.Done():
	}
	select {
	case <-r.release:
	case <-time.After(5 * time.Second):
	}
	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
	return "ok", nil
}

func newLaneRunner() *laneRunner {
	return &laneRunner{seen: make(chan string, 16), release: make(chan struct{})}
}

func (r *laneRunner) drain(n int, timeout time.Duration) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		select {
		case name := <-r.seen:
			out = append(out, name)
		case <-time.After(timeout):
			return out
		}
	}
	return out
}

func (r *laneRunner) peakInFlight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peak
}

// Two lanes are independent: an operation in one does not wait for the other.
func TestDistinctLanesRunConcurrently(t *testing.T) {
	runner := newLaneRunner()
	mgr := NewWithBuffer(runner, 16)
	mgr.SetConcurrencyLimit(8)
	mgr.AddSequential(Operation{ID: "file-1", Spec: Spec{Type: "browser_click", Args: map[string]any{"name": "file-1"}}}, "file")
	mgr.AddSequential(Operation{ID: "browser-1", Spec: Spec{Type: "browser_click", Args: map[string]any{"name": "browser-1"}}}, "browser")
	// Both must announce themselves while neither is allowed to finish, which is
	// only possible if they are running at the same time.
	got := runner.drain(2, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("only %v started within the window", got)
	}
	close(runner.release)
	for i := 0; i < 2; i++ {
		select {
		case <-mgr.Results():
		case <-time.After(5 * time.Second):
			t.Fatal("result timeout")
		}
	}
}

// One lane is exclusion with order: the second operation must not start until
// the first has finished, and it must be the second that arrives.
func TestOneLaneIsExclusiveAndOrdered(t *testing.T) {
	runner := newLaneRunner()
	mgr := NewWithBuffer(runner, 16)
	mgr.SetConcurrencyLimit(8)
	for _, name := range []string{"a", "b", "c"} {
		mgr.AddSequential(Operation{ID: name, Spec: Spec{Type: "browser_click", Args: map[string]any{"name": name}}}, "file")
	}
	// Only the first may start while nothing is released.
	if got := runner.drain(1, 500*time.Millisecond); len(got) != 1 || got[0] != "a" {
		t.Fatalf("first to start = %v, want [a]", got)
	}
	if peak := runner.peakInFlight(); peak != 1 {
		t.Fatalf("%d ran at once inside one lane, want 1", peak)
	}
	close(runner.release)
	rest := runner.drain(2, 3*time.Second)
	if len(rest) != 2 || rest[0] != "b" || rest[1] != "c" {
		t.Fatalf("order after a = %v, want [b c]", rest)
	}
	for i := 0; i < 3; i++ {
		select {
		case <-mgr.Results():
		case <-time.After(5 * time.Second):
			t.Fatal("result timeout")
		}
	}
}

// An unnamed queue is the default lane, so a tool that names nothing still
// serialises against everything else on it.
func TestUnnamedQueueFallsBackToTheDefaultLane(t *testing.T) {
	runner := newLaneRunner()
	mgr := NewWithBuffer(runner, 16)
	mgr.SetConcurrencyLimit(8)
	mgr.AddSequential(Operation{ID: "a", Spec: Spec{Type: "browser_click", Args: map[string]any{"name": "a"}}}, "")
	mgr.AddSequential(Operation{ID: "b", Spec: Spec{Type: "browser_click", Args: map[string]any{"name": "b"}}}, "")
	if got := runner.drain(1, 500*time.Millisecond); len(got) != 1 || got[0] != "a" {
		t.Fatalf("first to start = %v, want [a]", got)
	}
	if peak := runner.peakInFlight(); peak != 1 {
		t.Fatalf("%d ran at once on the default lane, want 1", peak)
	}
	close(runner.release)
	runner.drain(1, 3*time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-mgr.Results():
		case <-time.After(5 * time.Second):
			t.Fatal("result timeout")
		}
	}
}

// A lane takes a slot of the shared budget while it runs, so lanes cannot
// multiply the concurrency limit.
func TestLanesRespectTheSharedConcurrencyLimit(t *testing.T) {
	runner := newLaneRunner()
	mgr := NewWithBuffer(runner, 32)
	mgr.SetConcurrencyLimit(2)
	for _, lane := range []string{"a", "b", "c"} {
		for i := 0; i < 4; i++ {
			name := lane + string(rune('0'+i))
			mgr.AddSequential(Operation{ID: name, Spec: Spec{Type: "browser_click", Args: map[string]any{"name": name}}}, lane)
		}
	}
	// Wait for all three lanes to be occupied, then nothing more may start.
	runner.drain(2, 3*time.Second)
	time.Sleep(150 * time.Millisecond)
	if peak := runner.peakInFlight(); peak > 2 {
		t.Fatalf("peak concurrency %d, want at most the limit of 2", peak)
	}
	close(runner.release)
	for i := 0; i < 12; i++ {
		select {
		case <-mgr.Results():
		case <-time.After(5 * time.Second):
			t.Fatal("result timeout")
		}
	}
}
