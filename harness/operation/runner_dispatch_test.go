package operation

import (
	"context"
	"strings"
	"testing"
	"time"
)

// stubRunner stands in for the registry-backed runner.
type stubRunner struct {
	seen []string
	fail error
}

func (s *stubRunner) RunTool(ctx context.Context, spec Spec) (string, error) {
	s.seen = append(s.seen, spec.Type)
	if s.fail != nil {
		return "", s.fail
	}
	return "ran " + spec.Type, nil
}

// TestUnknownTypeIsDispatchedThroughRunner pins that an in-process tool is
// dispatched by the configured runner instead of being rejected by name.
//
// The executor used to carry a hardcoded list of type names, so a tool added
// later was refused with "unknown operation type" even though the registry knew
// how to run it. That is how a newly added browser tool was offered to the model
// and then failed on every single call.
func TestUnknownTypeIsDispatchedThroughRunner(t *testing.T) {
	runner := &stubRunner{}
	manager := NewWithBuffer(runner, 8)
	manager.Add(Operation{ID: "op1", Spec: Spec{Type: "some_new_tool", Args: map[string]any{}}, Status: StatusRunning})

	select {
	case result := <-manager.Results():
		if result.Status != StatusCompleted {
			t.Fatalf("status = %v, error = %q", result.Status, result.Error)
		}
		if !strings.Contains(result.Output, "ran some_new_tool") {
			t.Fatalf("output = %q", result.Output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no result")
	}
	if len(runner.seen) != 1 || runner.seen[0] != "some_new_tool" {
		t.Fatalf("runner saw %v", runner.seen)
	}
}

// TestUnknownTypeWithoutRunnerStillFails pins that the fallback error is kept
// when nothing can run the operation, so a genuine typo is still reported.
func TestUnknownTypeWithoutRunnerStillFails(t *testing.T) {
	manager := &LocalManager{results: make(chan ResultEvent, 4), cancels: map[string]context.CancelCauseFunc{}}
	manager.Add(Operation{ID: "op1", Spec: Spec{Type: "nope"}, Status: StatusRunning})

	select {
	case result := <-manager.Results():
		if result.Status != StatusFailed {
			t.Fatalf("status = %v", result.Status)
		}
		if !strings.Contains(result.Error, "unknown operation type") {
			t.Fatalf("error = %q", result.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no result")
	}
}

// TestRunnerFailureIsReportedAsToolError pins that a failing in-process tool
// surfaces as an ordinary tool error.
func TestRunnerFailureIsReportedAsToolError(t *testing.T) {
	manager := NewWithBuffer(&stubRunner{fail: errStub("chrome exploded")}, 4)
	manager.Add(Operation{ID: "op1", Spec: Spec{Type: "browser"}, Status: StatusRunning})

	select {
	case result := <-manager.Results():
		if result.Status != StatusFailed {
			t.Fatalf("status = %v", result.Status)
		}
		if !strings.Contains(result.Error, "chrome exploded") {
			t.Fatalf("error = %q", result.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no result")
	}
}

type stubErr string

func (e stubErr) Error() string { return string(e) }

func errStub(msg string) error { return stubErr(msg) }
