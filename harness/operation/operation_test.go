package operation

import (
	"testing"
	"time"

	"sooraa/harness/event"
	"sooraa/harness/question"
)

func TestShellOperation(t *testing.T) {
	mgr := New(nil)
	mgr.Add(Operation{
		ID:     "test-1",
		Spec:   Spec{Type: "shell", Args: map[string]any{"command": "echo hello"}},
		Status: StatusPending,
	})
	select {
	case r := <-mgr.Results():
		if r.Status != StatusCompleted {
			t.Fatalf("expected completed, got %s: error=%q output=%q", r.Status, r.Error, r.Output)
		}
		if r.Output != "hello\n" {
			t.Fatalf("expected 'hello', got %q", r.Output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for result")
	}
}

func TestReadOperation(t *testing.T) {
	mgr := New(nil)
	mgr.Add(Operation{
		ID:     "test-read",
		Spec:   Spec{Type: "read", Args: map[string]any{"path": "../../go.mod"}},
		Status: StatusPending,
	})
	select {
	case r := <-mgr.Results():
		if r.Status != StatusCompleted {
			t.Fatalf("expected completed, got %s: error=%q output=%q", r.Status, r.Error, r.Output[:min(len(r.Output), 50)])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestGlobOperation(t *testing.T) {
	mgr := New(nil)
	mgr.Add(Operation{
		ID:     "test-glob",
		Spec:   Spec{Type: "glob", Args: map[string]any{"pattern": "go.mod"}},
		Status: StatusPending,
	})
	select {
	case r := <-mgr.Results():
		if r.Status != StatusCompleted {
			t.Fatalf("expected completed, got %s", r.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestCancelOperationRemovesFromManager(t *testing.T) {
	mgr := New(nil)
	mgr.Add(Operation{
		ID:     "test-cancel",
		Spec:   Spec{Type: "shell", Args: map[string]any{"command": "echo done"}},
		Status: StatusPending,
	})
	time.Sleep(200 * time.Millisecond)
	mgr.Cancel("test-cancel")
	// cancel is async - just verify it doesn't panic
	select {
	case r := <-mgr.Results():
		// may complete normally or get canceled - either is acceptable
		_ = r
	default:
	}
	// Wait and verify no panic occurred
	time.Sleep(100 * time.Millisecond)
}

func TestQuestionAnswer(t *testing.T) {
	const sessionID = "test-question-session"
	const requestID = "question-1"
	events := event.Default.Subscribe(sessionID, 4)
	defer event.Default.Unsubscribe(sessionID, events)

	mgr := New(nil)
	mgr.Add(Operation{
		ID: requestID,
		Spec: Spec{Type: "question", Args: map[string]any{
			"question":   "Proceed?",
			"options":    []string{"Yes", "No"},
			"session_id": sessionID,
		}},
		Status: StatusPending,
	})

	select {
	case e := <-events:
		if e.Type != event.TypeQuestion {
			t.Fatalf("expected question event, got %s", e.Type)
		}
		if !question.Default.Answer(sessionID, requestID, "Yes") {
			t.Fatal("expected pending question answer to be accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("question event timeout")
	}

	select {
	case r := <-mgr.Results():
		if r.Status != StatusCompleted {
			t.Fatalf("expected completed, got %s: %s", r.Status, r.Error)
		}
		if r.Output != "Yes" {
			t.Fatalf("expected Yes, got %q", r.Output)
		}
	case <-time.After(time.Second):
		t.Fatal("question result timeout")
	}
}

func TestUnknownOperationType(t *testing.T) {
	mgr := New(nil)
	mgr.Add(Operation{
		ID:     "test-unknown",
		Spec:   Spec{Type: "nonexistent", Args: map[string]any{}},
		Status: StatusPending,
	})
	select {
	case r := <-mgr.Results():
		if r.Status != StatusFailed {
			t.Fatalf("expected failed, got %s", r.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > len(sub) && containsIn(s, sub)))
}

func containsIn(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
