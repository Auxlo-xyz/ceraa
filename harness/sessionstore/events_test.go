package sessionstore

import (
	"testing"
	"time"
)

func TestRunEventsRoundTripAndProjectLatest(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	sess := &Session{ID: "events", Version: schemaVersion}
	sess.AppendRunEvent(RunEvent{Type: RunEventStarted, RunID: "run1", Status: RunRunning, CreatedAt: now})
	sess.AppendRunEvent(RunEvent{Type: RunEventFailed, RunID: "run1", Status: RunFailed, CreatedAt: now.Add(time.Second), Error: "provider stopped"})
	if sess.RunEvents[0].Seq != 1 || sess.RunEvents[1].Seq != 2 {
		t.Fatalf("event sequences = %d, %d", sess.RunEvents[0].Seq, sess.RunEvents[1].Seq)
	}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("events")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := loaded.CurrentRunEvent("run1")
	if !ok || got.Type != RunEventFailed || got.Error != "provider stopped" {
		t.Fatalf("latest event = %+v, found=%t", got, ok)
	}
	if _, ok := loaded.CurrentRunEvent("missing"); ok {
		t.Fatal("missing run unexpectedly projected")
	}
}

func TestRunEventsReplayAndProjectAreReadOnly(t *testing.T) {
	sess := &Session{}
	sess.AppendRunEvent(RunEvent{Type: RunEventStarted, RunID: "run1", Status: RunRunning})
	sess.AppendRunEvent(RunEvent{Type: RunEventStep, RunID: "run1", Status: RunRunning, Step: 1})
	sess.AppendRunEvent(RunEvent{Type: RunEventCompleted, RunID: "run2", Status: RunCompleted})

	replay := sess.ReplayRunEvents("run1")
	if len(replay) != 2 || replay[0].Seq != 1 || replay[1].Seq != 2 {
		t.Fatalf("replay = %+v", replay)
	}
	replay[0].Status = RunFailed
	if sess.RunEvents[0].Status != RunRunning {
		t.Fatal("replay changed session history")
	}

	projected := ProjectRunEvents(sess.RunEvents)
	if projected["run1"].Step != 1 || projected["run2"].Status != RunCompleted {
		t.Fatalf("projection = %+v", projected)
	}
	projected["run1"] = RunEvent{Status: RunFailed}
	if sess.RunEvents[1].Status != RunRunning {
		t.Fatal("projection changed session history")
	}
}

func TestRunEventsAppendSequenceContinuesAfterLegacyEvents(t *testing.T) {
	sess := &Session{RunEvents: []RunEvent{{Type: RunEventStarted, RunID: "run1"}, {Seq: 7, Type: RunEventStep, RunID: "run1"}}}
	sess.AppendRunEvent(RunEvent{Type: RunEventFailed, RunID: "run1", Status: RunFailed})
	if got := sess.RunEvents[len(sess.RunEvents)-1].Seq; got != 8 {
		t.Fatalf("next sequence = %d, want 8", got)
	}
}

func TestRunEventsRecoverEmptyHistory(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{ID: "legacy", Version: 3, Runs: []Run{{ID: "run1", Status: RunRunning}}}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if changed := Recover(loaded); !changed {
		t.Fatal("running legacy run was not recovered")
	}
	if got := loaded.Runs[0].Status; got != RunInterrupted {
		t.Fatalf("recovered run status = %q", got)
	}
	event, ok := loaded.CurrentRunEvent("run1")
	if !ok || event.Type != RunEventInterrupted || event.Status != RunInterrupted {
		t.Fatalf("recovery event = %+v, found=%t", event, ok)
	}
	if loaded.RunEvents == nil {
		t.Fatal("legacy session did not initialize empty run events")
	}
	if changed := Recover(loaded); changed {
		t.Fatal("second recovery changed an already recovered session")
	}
}
