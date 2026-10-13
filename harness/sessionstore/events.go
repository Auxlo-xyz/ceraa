package sessionstore

import (
	"sort"
	"time"
)

// RunEvent is a durable, secret-free record of a run lifecycle transition.
// It is intentionally smaller than a full operation or message so it can be
// projected after a restart without copying model or tool payloads.
type RunEvent struct {
	Seq       int       `json:"seq,omitempty"`
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

// ReplayRunEvents returns a read-only copy of one run's event history in event
// order. The returned slice can be changed by the caller without changing the
// session.
func (s *Session) ReplayRunEvents(runID string) []RunEvent {
	if s == nil || runID == "" {
		return nil
	}
	events := make([]RunEvent, 0)
	for _, event := range s.RunEvents {
		if event.RunID == runID {
			events = append(events, event)
		}
	}
	sortRunEvents(events)
	return events
}

// ProjectRunEvents returns the latest event for each run without mutating the
// event history. Sequence IDs are authoritative when present; legacy events
// without a sequence ID retain their original order.
func ProjectRunEvents(events []RunEvent) map[string]RunEvent {
	ordered := append([]RunEvent(nil), events...)
	sortRunEvents(ordered)
	projected := make(map[string]RunEvent)
	for _, event := range ordered {
		if event.RunID != "" {
			projected[event.RunID] = event
		}
	}
	return projected
}

// CurrentRunEvent returns the latest event for a run, or false when the run
// has no event history. Events are append-only, so the last matching event is
// the current projection.
func (s *Session) CurrentRunEvent(runID string) (RunEvent, bool) {
	latest, ok := ProjectRunEvents(s.RunEvents)[runID]
	return latest, ok && runID != ""
}

// AppendRunEvent adds one secret-free lifecycle transition to the session.
// Sequence IDs are assigned here rather than trusted from callers so every
// newly appended event is strictly monotonic within the session.
func (s *Session) AppendRunEvent(event RunEvent) {
	if s == nil {
		return
	}
	event.Seq = nextRunEventSeq(s.RunEvents)
	s.RunEvents = append(s.RunEvents, event)
}

func nextRunEventSeq(events []RunEvent) int {
	max := 0
	for _, event := range events {
		if event.Seq > max {
			max = event.Seq
		}
	}
	return max + 1
}

func sortRunEvents(events []RunEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Seq == 0 || events[j].Seq == 0 {
			return events[i].Seq == 0 && events[j].Seq != 0
		}
		return events[i].Seq < events[j].Seq
	})
}
