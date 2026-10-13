package sessionstore

import "time"

// RunEvent is a durable, secret-free record of a run lifecycle transition.
// It is intentionally smaller than a full operation or message so it can be
// projected after a restart without copying model or tool payloads.
type RunEvent struct {
	Type      string    `json:"type"`
	RunID     string    `json:"run_id"`
	Status    RunStatus `json:"status,omitempty"`
	Step      int       `json:"step,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Error     string    `json:"error,omitempty"`
}

const (
	RunEventStarted     = "run.started"
	RunEventCompleted   = "run.completed"
	RunEventFailed      = "run.failed"
	RunEventCanceled    = "run.cancelled"
	RunEventInterrupted = "run.interrupted"
	RunEventStep        = "run.step"
)

// CurrentRunEvent returns the latest event for a run, or false when the run
// has no event history. Events are append-only, so the last matching event is
// the current projection.
func (s *Session) CurrentRunEvent(runID string) (RunEvent, bool) {
	if s == nil || runID == "" {
		return RunEvent{}, false
	}
	var latest RunEvent
	found := false
	for _, event := range s.RunEvents {
		if event.RunID == runID {
			latest = event
			found = true
		}
	}
	return latest, found
}

// AppendRunEvent adds one secret-free lifecycle transition to the session.
func (s *Session) AppendRunEvent(event RunEvent) {
	s.RunEvents = append(s.RunEvents, event)
}
