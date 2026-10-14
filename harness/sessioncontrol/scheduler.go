package sessioncontrol

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ceraa/harness/scheduler"
)

// SchedulerFacade is the narrow bridge from durable scheduler control to the
// existing session-control launch seam. It does not create or own a
// coordinator. The scheduler Store remains responsible for claiming jobs and
// fencing completion with Job.AttemptID.
type SchedulerFacade interface {
	List(context.Context) ([]scheduler.Job, error)
	Add(context.Context, scheduler.Job) error
	Remove(context.Context, string) (bool, error)
	Run(context.Context, time.Duration) error
	Execute(context.Context, scheduler.Job) error
}

// CoordinatorLauncher is the coordinator-owned seam for launching a claimed
// job. It deliberately accepts only the user text: the scheduler retains
// attempt fencing, while the coordinator retains session locking.
type CoordinatorLauncher interface {
	Launch(context.Context, string) error
}

// NewSchedulerFacadeWithCoordinator wires a scheduler to an existing
// coordinator-owned launcher. It is separate from NewSchedulerFacade so
// transports can defer production injection without constructing a duplicate
// coordinator.
func NewSchedulerFacadeWithCoordinator(store *scheduler.Store, launcher CoordinatorLauncher) (SchedulerFacade, error) {
	if launcher == nil {
		return NewSchedulerFacade(store, nil)
	}
	return NewSchedulerFacade(store, func(ctx context.Context, req LaunchRequest) (LaunchResult, error) {
		return LaunchResult{}, launcher.Launch(ctx, req.User)
	})
}

type schedulerFacade struct {
	store  *scheduler.Store
	launch LaunchFunc
}

// NewSchedulerFacade maps scheduler control and execution onto the shared
// session launch seam. A nil launch is allowed at construction so read-only
// schedule control remains usable; Run and Execute return ErrNoLauncher.
func NewSchedulerFacade(store *scheduler.Store, launch LaunchFunc) (SchedulerFacade, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: scheduler store is required", ErrInvalidRequest)
	}
	return &schedulerFacade{store: store, launch: launch}, nil
}

func (f *schedulerFacade) List(_ context.Context) ([]scheduler.Job, error) {
	return f.store.List()
}

func (f *schedulerFacade) Add(_ context.Context, job scheduler.Job) error {
	return f.store.Add(job)
}

func (f *schedulerFacade) Remove(_ context.Context, id string) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, fmt.Errorf("%w: scheduler job id is required", ErrInvalidRequest)
	}
	return f.store.Remove(id)
}

func (f *schedulerFacade) Run(ctx context.Context, interval time.Duration) error {
	if f.launch == nil {
		return ErrNoLauncher
	}
	return f.store.RunWithInterval(ctx, interval, f.Execute)
}

func (f *schedulerFacade) Execute(ctx context.Context, job scheduler.Job) error {
	if f.launch == nil {
		return ErrNoLauncher
	}
	if strings.TrimSpace(job.SessionID) == "" || strings.TrimSpace(job.AttemptID) == "" {
		return fmt.Errorf("%w: scheduled job session and attempt id are required", ErrInvalidRequest)
	}
	_, err := f.launch(ctx, LaunchRequest{
		SessionID: SessionID(job.SessionID),
		User:      job.Prompt,
		AttemptID: AttemptID(job.AttemptID),
		Envelope: RequestEnvelope{
			ClientID:  "scheduler:" + job.ID,
			RequestID: job.AttemptID,
		},
	})
	return err
}

var _ SchedulerFacade = (*schedulerFacade)(nil)
