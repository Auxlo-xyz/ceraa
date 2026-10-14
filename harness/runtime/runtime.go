// Package runtime assembles the core Ceraa harness dependencies for one run.
// It keeps construction in one small object without introducing a dependency
// injection framework or changing service ownership rules.
package runtime

import (
	"context"

	"ceraa/harness/coordinator"
	"ceraa/harness/event"
	"ceraa/harness/inbox"
	"ceraa/harness/operation"
	"ceraa/harness/sessioncontrol"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// Runtime is the assembled core harness for one execution surface.
type Runtime struct {
	Sessions    *sessionstore.Store
	Tools       *tool.Registry
	Operations  *operation.LocalManager
	Coordinator *coordinator.Runner
	// Launcher is owned by the coordinator and is the safe injection point for
	// scheduler execution. It is not started here.
	Launcher sessioncontrol.LaunchFunc
	Control  sessioncontrol.Service
}

// New assembles the core harness with explicit dependency ownership.
func New(cfg coordinator.RunnerConfig, sessions *sessionstore.Store, tools *tool.Registry) *Runtime {
	coordinatorRunner := coordinator.New(cfg, sessions, tools)
	launch := func(ctx context.Context, req sessioncontrol.LaunchRequest) (sessioncontrol.LaunchResult, error) {
		return sessioncontrol.LaunchResult{}, coordinatorRunner.Launch(ctx, req.User)
	}
	control, err := sessioncontrol.New(sessioncontrol.Dependencies{
		Store:  sessions,
		Bus:    event.Default,
		Launch: launch,
		Inbox: func(sessionID string) (*inbox.Inbox, bool) {
			if cfg.Inbox == nil || sessionID != cfg.SessionID {
				return nil, false
			}
			return cfg.Inbox, true
		},
	})
	if err != nil {
		panic(err)
	}
	return &Runtime{
		Sessions:    sessions,
		Tools:       tools,
		Operations:  coordinatorRunner.Operations(),
		Coordinator: coordinatorRunner,
		Launcher:    launch,
		Control:     control,
	}
}
