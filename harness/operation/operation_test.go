package operation

import (
	"testing"
	"time"

	"ceraa/harness/event"
	"ceraa/harness/question"
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

// The glob, grep, ls and catalog operation types were removed along with their
// tool schemas. They used to stay reachable by name alone, so a model that
// hallucinated a retired tool name still got real work done: a `ls` call returned
// a genuine directory listing. The operation layer and the tool layer have to
// agree on what exists, or the schema stops describing the surface.
func TestRemovedOperationTypesAreRejected(t *testing.T) {
	for _, spec := range []Spec{
		{Type: "glob", Args: map[string]any{"pattern": "go.mod"}},
		{Type: "grep", Args: map[string]any{"pattern": "func main"}},
		{Type: "ls", Args: map[string]any{"path": "."}},
		{Type: "skills_catalog", Args: map[string]any{}},
		{Type: "command_catalog", Args: map[string]any{}},
		{Type: "mcp_catalog", Args: map[string]any{}},
	} {
		mgr := New(nil)
		mgr.Add(Operation{ID: "t-" + spec.Type, Spec: spec, Status: StatusPending})
		select {
		case r := <-mgr.Results():
			if r.Status == StatusCompleted {
				t.Errorf("removed operation type %q still executed (output %q)", spec.Type, r.Output)
			}
			if r.Error == "" {
				t.Errorf("removed operation type %q failed without saying why", spec.Type)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: timeout", spec.Type)
		}
	}
}

// The types that remain must keep working; the removal above is only safe
// because these are untouched.
func TestRetainedOperationTypesStillRun(t *testing.T) {
	mgr := New(nil)
	mgr.Add(Operation{
		ID:     "test-read",
		Spec:   Spec{Type: "read", Args: map[string]any{"path": "operation.go"}},
		Status: StatusPending,
	})
	select {
	case r := <-mgr.Results():
		if r.Status != StatusCompleted {
			t.Fatalf("read failed: %s %s", r.Status, r.Error)
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

func TestCancelStopsShellOperation(t *testing.T) {
	mgr := New(nil)
	mgr.Add(Operation{
		ID:     "test-cancel-shell",
		Spec:   Spec{Type: "shell", Args: map[string]any{"command": "sleep 5"}},
		Status: StatusPending,
	})
	time.Sleep(100 * time.Millisecond)
	mgr.Cancel("test-cancel-shell")
	select {
	case r := <-mgr.Results():
		if r.Status != StatusCanceled && r.Status != StatusFailed {
			t.Fatalf("expected canceled or failed, got %s", r.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shell operation did not stop after cancellation")
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
