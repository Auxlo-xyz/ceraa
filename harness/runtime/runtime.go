// Package runtime assembles the core Ceraa harness dependencies for one run.
// It keeps construction in one small object without introducing a dependency
// injection framework or changing service ownership rules.
package runtime

import (
	"ceraa/harness/coordinator"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// Runtime is the assembled core harness for one execution surface.
//
// The transport owns the Runtime lifetime. The coordinator owns the operation
// manager and all run lifecycle behavior after construction.
type Runtime struct {
	Sessions    *sessionstore.Store
	Tools       *tool.Registry
	Operations  *operation.LocalManager
	Coordinator *coordinator.Runner
}

// New assembles the core harness with explicit dependency ownership.
func New(cfg coordinator.RunnerConfig, sessions *sessionstore.Store, tools *tool.Registry) *Runtime {
	coordinatorRunner := coordinator.New(cfg, sessions, tools)
	return &Runtime{
		Sessions:    sessions,
		Tools:       tools,
		Operations:  coordinatorRunner.Operations(),
		Coordinator: coordinatorRunner,
	}
}
