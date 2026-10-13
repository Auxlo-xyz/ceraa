package sessionstore

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRunEventStorageStaysCompact(t *testing.T) {
	events := make([]RunEvent, 0, 100)
	for i := 0; i < 100; i++ {
		events = append(events, RunEvent{
			Type:      RunEventStep,
			RunID:     "run_1234567890",
			Status:    RunRunning,
			Step:      i,
			CreatedAt: time.Unix(int64(i), 0).UTC(),
		})
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 20*1024 {
		t.Fatalf("100 lifecycle events used %d bytes", len(encoded))
	}
}

func TestRunEventProjectionMatchesRunStatus(t *testing.T) {
	sess := &Session{Runs: []Run{{ID: "run1", Status: RunFailed}}}
	sess.AppendRunEvent(RunEvent{Type: RunEventStarted, RunID: "run1", Status: RunRunning})
	sess.AppendRunEvent(RunEvent{Type: RunEventFailed, RunID: "run1", Status: RunFailed})
	event, ok := sess.CurrentRunEvent("run1")
	if !ok || event.Status != sess.Runs[0].Status {
		t.Fatalf("event status %q does not match run status %q", event.Status, sess.Runs[0].Status)
	}
}
