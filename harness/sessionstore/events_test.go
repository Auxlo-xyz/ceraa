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
