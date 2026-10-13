package coordinator

import "ceraa/harness/sessionstore"

// RunOutcome describes what a completed coordinator run established.
type RunOutcome string

const (
	RunOutcomePending      RunOutcome = "pending"
	RunOutcomeSucceeded    RunOutcome = "succeeded"
	RunOutcomeFailed       RunOutcome = "failed"
	RunOutcomeInconclusive RunOutcome = "inconclusive"
	RunOutcomeBlocked      RunOutcome = "blocked"
)

// RunAction identifies the next action implied by a terminal run outcome.
type RunAction string

const (
	RunActionNone   RunAction = "none"
	RunActionRetry  RunAction = "retry"
	RunActionUser   RunAction = "user"
	RunActionReview RunAction = "review"
)

// RunLifecycle is the typed terminal projection of a coordinator run.
// It is intentionally separate from durable session events: callers can decide
// how to present or persist it without changing the event sequence.
type RunLifecycle struct {
	Status         sessionstore.RunStatus
	Outcome        RunOutcome
	ActionRequired RunAction
	Err            error
}

// ClassifyRun maps the coordinator's status vocabulary to an explicit outcome
// and next action. The mapping keeps cancellation and interruption distinct from
// provider or tool failure, so callers do not have to infer intent from errors.
func ClassifyRun(status sessionstore.RunStatus, runErr error) RunLifecycle {
	lifecycle := RunLifecycle{Status: status, Err: runErr}
	switch status {
	case sessionstore.RunCompleted:
		lifecycle.Outcome = RunOutcomeSucceeded
		lifecycle.ActionRequired = RunActionNone
	case sessionstore.RunCanceled:
		lifecycle.Outcome = RunOutcomeBlocked
		lifecycle.ActionRequired = RunActionUser
	case sessionstore.RunInterrupted:
		lifecycle.Outcome = RunOutcomeInconclusive
		lifecycle.ActionRequired = RunActionReview
	case sessionstore.RunFailed:
		lifecycle.Outcome = RunOutcomeFailed
		lifecycle.ActionRequired = RunActionRetry
	default:
		lifecycle.Outcome = RunOutcomePending
		lifecycle.ActionRequired = RunActionNone
	}
	return lifecycle
}
