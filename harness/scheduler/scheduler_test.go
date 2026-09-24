package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAddListRemoveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC)
	store.Now = func() time.Time { return base }
	if err := store.Add(Job{ID: "daily", Cron: "0 9 * * *", Timezone: "UTC", Prompt: "run"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(Job{ID: "once", RunAt: base.Add(time.Hour), Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	jobs, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != "daily" || jobs[0].NextRunAt != time.Date(2026, time.January, 2, 9, 0, 0, 0, time.UTC) {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
	removed, err := store.Remove("once")
	if err != nil || !removed {
		t.Fatalf("expected remove: removed=%v err=%v", removed, err)
	}
	if removed, err := store.Remove("once"); err != nil || removed {
		t.Fatalf("unexpected second remove: removed=%v err=%v", removed, err)
	}

	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reloaded.Get("daily"); err != nil {
		t.Fatal(err)
	}
}

func TestNextRunSupportsIntervalsAndCron(t *testing.T) {
	base := time.Date(2026, time.January, 2, 3, 4, 30, 0, time.UTC)
	next, err := NextRun(Job{Interval: 10 * time.Minute}, base)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Equal(base.Add(10 * time.Minute)) {
		t.Fatalf("unexpected interval run: %s", next)
	}
	next, err = NextRun(Job{Cron: "0 9 * * *", Timezone: "UTC"}, base)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Equal(time.Date(2026, time.January, 2, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected cron run: %s", next)
	}
	next, err = NextRun(Job{RunAt: base.Add(-time.Second)}, base)
	if err != nil {
		t.Fatal(err)
	}
	if !next.IsZero() {
		t.Fatalf("expected no next one-time run, got %s", next)
	}
}

func TestClaimDueProtectsExecutionAndPersistsResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.January, 2, 3, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { return base }
	if err := store.Add(Job{ID: "job", Interval: time.Minute, NextRunAt: base}); err != nil {
		t.Fatal(err)
	}

	claimed, err := store.ClaimDue(base)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("expected one claim: jobs=%+v err=%v", claimed, err)
	}
	if claimed[0].LastStatus != StatusRunning || claimed[0].RunCount != 1 {
		t.Fatalf("unexpected claimed job: %+v", claimed[0])
	}
	claimedAgain, err := store.ClaimDue(base.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(claimedAgain) != 0 {
		t.Fatalf("job was claimed while running: %+v", claimedAgain)
	}
	if err := store.MarkError("job", errors.New("failed")); err != nil {
		t.Fatal(err)
	}
	loaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	job, err := loaded.Get("job")
	if err != nil {
		t.Fatal(err)
	}
	if job.LastStatus != StatusError || job.LastError != "failed" || job.NextRunAt != base.Add(time.Minute) {
		t.Fatalf("unexpected persisted result: %+v", job)
	}
}

func TestRunnerLoopPassesContextAndPersistsCallbackError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	store.Now = func() time.Time { return base }
	if err := store.Add(Job{ID: "job", Interval: time.Hour, NextRunAt: base}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan struct{})
	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- store.RunWithInterval(ctx, 5*time.Millisecond, func(runCtx context.Context, job Job) error {
			if runCtx != ctx {
				t.Errorf("runner did not pass context")
			}
			close(called)
			return errors.New("callback failed")
		})
	}()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("callback was not called")
	}
	job := waitForJob(t, store, "job", StatusError)
	if job.LastError != "callback failed" {
		t.Fatalf("unexpected callback error: %+v", job)
	}
	cancel()
	select {
	case err := <-runnerDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not stop")
	}
}

func waitForJob(t *testing.T, store *Store, id string, status Status) Job {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		job, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if job.LastStatus == status {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job %s did not reach %s", id, status)
	return Job{}
}
