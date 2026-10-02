package operation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RunSync is what a script calls, and it is a different path from the queue:
// inline, no slot, no result event. Each of those differences is a way to be
// wrong, so each is pinned here rather than inferred from the shared dispatch.

func TestRunSyncRunsANativeOperationInline(t *testing.T) {
	manager := NewWithBuffer(nil, 8)
	out, err := manager.RunSync(context.Background(), Spec{
		Type: "shell",
		Args: map[string]any{"command": "printf 'inline'"},
	})
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if !strings.Contains(out, "inline") {
		t.Fatalf("output = %q", out)
	}
}

func TestRunSyncReadsAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewWithBuffer(nil, 8)
	out, err := manager.RunSync(context.Background(), Spec{
		Type: "read",
		Args: map[string]any{"path": path},
	})
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if !strings.Contains(out, "line one") {
		t.Fatalf("output = %q", out)
	}
}

// A type the manager does not know natively belongs to the runner. If RunSync
// skipped that fallthrough, every in-process tool a script called would fail
// with "unknown operation type" while the same call outside a script worked.
func TestRunSyncFallsThroughToTheRunner(t *testing.T) {
	runner := &stubRunner{}
	manager := NewWithBuffer(runner, 8)
	out, err := manager.RunSync(context.Background(), Spec{Type: "some_new_tool", Args: map[string]any{}})
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if !strings.Contains(out, "ran some_new_tool") {
		t.Fatalf("output = %q", out)
	}
	if len(runner.seen) != 1 || runner.seen[0] != "some_new_tool" {
		t.Fatalf("runner saw %v", runner.seen)
	}
}

// A failure has to come back as an error, because a script turns it into a throw
// the model can correct itself from. Returning it as a successful empty string
// would read as the tool having done nothing.
func TestRunSyncReportsAFailureAsAnError(t *testing.T) {
	runner := &stubRunner{fail: errors.New("bad arguments")}
	manager := NewWithBuffer(runner, 8)
	if _, err := manager.RunSync(context.Background(), Spec{Type: "some_new_tool"}); err == nil {
		t.Fatal("expected the runner's failure to surface")
	} else if !strings.Contains(err.Error(), "bad arguments") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunSyncStopsWhenTheContextIsDone(t *testing.T) {
	manager := NewWithBuffer(nil, 8)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.RunSync(ctx, Spec{
		Type: "shell",
		Args: map[string]any{"command": "echo never"},
	}); err == nil {
		t.Fatal("expected the cancelled context to stop the operation")
	}
}

// RunSync is inline, so it must not publish to the results channel. A result
// nobody is waiting for there would be read later as if it belonged to a queued
// operation, pairing the wrong output with the wrong call id.
func TestRunSyncDoesNotPublishAResult(t *testing.T) {
	manager := NewWithBuffer(nil, 8)
	if _, err := manager.RunSync(context.Background(), Spec{
		Type: "shell",
		Args: map[string]any{"command": "true"},
	}); err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	select {
	case result := <-manager.Results():
		t.Fatalf("RunSync published a result of its own: %+v", result)
	case <-time.After(150 * time.Millisecond):
	}
}

// The nested call must not take a slot. The operation that owns the script
// already holds one, so an acquire here deadlocks as soon as the limit is one.
func TestRunSyncTakesNoConcurrencySlot(t *testing.T) {
	manager := NewWithBuffer(nil, 8)
	manager.SetConcurrencyLimit(1)
	done := make(chan error, 1)
	go func() {
		_, err := manager.RunSync(context.Background(), Spec{
			Type: "shell",
			Args: map[string]any{"command": "true"},
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunSync: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunSync blocked waiting for a slot it must not need")
	}
}

// Secrets are scrubbed on the way out of dispatch. A nested call has to get the
// same scrubbing a queued one does, because it lands in the same context.
func TestRunSyncScrubsSecrets(t *testing.T) {
	manager := NewWithBuffer(nil, 8)
	out, err := manager.RunSync(context.Background(), Spec{
		Type: "shell",
		Args: map[string]any{"command": "printf '%s' 'sk-test-abcdef1234567890'"},
	})
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if strings.Contains(out, "sk-test-abcdef1234567890") {
		t.Fatalf("a nested call returned a secret: %q", out)
	}
}

// The two paths share one dispatch function, and this is the property that share
// exists for. If they ever drift, a script and a plain tool call disagree about
// what the same operation does, and the difference would show up as a script
// returning something the model cannot trust.
func TestRunSyncProducesWhatTheQueuedPathProduces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Type: "read", Args: map[string]any{"path": path}}

	inline, err := NewWithBuffer(nil, 8).RunSync(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}

	queuedManager := NewWithBuffer(nil, 8)
	queuedManager.Add(Operation{ID: "queued-1", Spec: spec, Status: StatusRunning})
	var queued string
	select {
	case result := <-queuedManager.Results():
		if result.Error != "" {
			t.Fatalf("queued read failed: %s", result.Error)
		}
		queued = result.Output
	case <-time.After(5 * time.Second):
		t.Fatal("no queued result")
	}

	if inline != queued {
		t.Errorf("inline = %q\nqueued = %q", inline, queued)
	}
}
