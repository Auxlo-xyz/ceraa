package coordinator

import (
	"errors"
	"testing"

	"ceraa/harness/sessionstore"
)

func TestClassifyRunMapsStatusToOutcomeAndAction(t *testing.T) {
	tests := []struct {
		name    string
		status  sessionstore.RunStatus
		outcome RunOutcome
		action  RunAction
	}{
		{"completed", sessionstore.RunCompleted, RunOutcomeSucceeded, RunActionNone},
		{"failed", sessionstore.RunFailed, RunOutcomeFailed, RunActionRetry},
		{"canceled", sessionstore.RunCanceled, RunOutcomeBlocked, RunActionUser},
		{"interrupted", sessionstore.RunInterrupted, RunOutcomeInconclusive, RunActionReview},
		{"running", sessionstore.RunRunning, RunOutcomePending, RunActionNone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lifecycle := ClassifyRun(test.status, nil)
			if lifecycle.Outcome != test.outcome || lifecycle.ActionRequired != test.action {
				t.Fatalf("ClassifyRun(%q) = outcome %q action %q, want %q %q", test.status, lifecycle.Outcome, lifecycle.ActionRequired, test.outcome, test.action)
			}
			if lifecycle.Status != test.status {
				t.Fatalf("ClassifyRun(%q) changed status to %q", test.status, lifecycle.Status)
			}
		})
	}
}

func TestClassifyRunPreservesError(t *testing.T) {
	want := errors.New("provider unavailable")
	if got := ClassifyRun(sessionstore.RunFailed, want); got.Err != want {
		t.Fatalf("ClassifyRun did not preserve error: %v", got.Err)
	}
}
