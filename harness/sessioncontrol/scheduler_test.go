package sessioncontrol

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"ceraa/harness/scheduler"
)

func TestSchedulerFacadeMapsClaimedAttemptToSharedLaunch(t *testing.T) {
	store, err := scheduler.New(filepath.Join(t.TempDir(), "scheduler.json"))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.January, 2, 3, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { return base }
	if err := store.Add(scheduler.Job{ID: "job", SessionID: "session", Prompt: "scheduled", Interval: time.Minute, NextRunAt: base}); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDue(base)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %+v, err = %v", claimed, err)
	}

	var got LaunchRequest
	facade, err := NewSchedulerFacade(store, func(_ context.Context, req LaunchRequest) (LaunchResult, error) {
		got = req
		return LaunchResult{RunID: "run"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := facade.Execute(context.Background(), claimed[0]); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "session" || got.User != "scheduled" || got.AttemptID != AttemptID(claimed[0].AttemptID) {
		t.Fatalf("launch request = %+v, want session, prompt, and claimed attempt", got)
	}
	if got.Envelope.ClientID != "scheduler:job" || got.Envelope.RequestID != claimed[0].AttemptID {
		t.Fatalf("launch envelope = %+v", got.Envelope)
	}

	if err := store.FinishAttempt(claimed[0].ID, claimed[0].AttemptID, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishAttempt(claimed[0].ID, "stale", errors.New("late")); !errors.Is(err, scheduler.ErrAttemptConflict) {
		t.Fatalf("stale completion = %v, want attempt conflict", err)
	}
}

func TestSchedulerFacadeKeepsControlWithoutLauncher(t *testing.T) {
	store, err := scheduler.New(filepath.Join(t.TempDir(), "scheduler.json"))
	if err != nil {
		t.Fatal(err)
	}
	facade, err := NewSchedulerFacade(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := facade.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := facade.Run(context.Background(), time.Second); !errors.Is(err, ErrNoLauncher) {
		t.Fatalf("Run = %v, want ErrNoLauncher", err)
	}
}

func TestSchedulerFacadeRejectsUnclaimedExecution(t *testing.T) {
	store, err := scheduler.New(filepath.Join(t.TempDir(), "scheduler.json"))
	if err != nil {
		t.Fatal(err)
	}
	facade, err := NewSchedulerFacade(store, func(context.Context, LaunchRequest) (LaunchResult, error) { return LaunchResult{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := facade.Execute(context.Background(), scheduler.Job{ID: "job", SessionID: "session"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Execute = %v, want invalid request", err)
	}
}
