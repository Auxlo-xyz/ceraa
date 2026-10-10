package internal

import (
	"ceraa/harness/instruction"
)

// InstructionRules returns the loader a RunnerConfig wants for the compiled
// project rules at root.
//
// Refresh-then-Snapshot rather than Snapshot alone, because a store's
// constructor reads nothing on purpose and Snapshot answers with the last good
// compile. A caller wiring only Snapshot would get an empty rule set forever on
// a project that has an AGENTS.md, which is the failure that looks like a
// project with no rules.
//
// The loader is called exactly once per run by the runner, so the refresh here is
// once per run too: an edit made mid-run is picked up by the next run rather than
// landing two revisions of the same rule in one transcript. That is the same
// boundary the bond's once-per-run render uses and for the same reason.
func InstructionRules(root string) func() instruction.Snapshot {
	store := instruction.NewStore(root, instruction.DefaultFile)
	return func() instruction.Snapshot {
		store.Refresh()
		return store.Snapshot()
	}
}
