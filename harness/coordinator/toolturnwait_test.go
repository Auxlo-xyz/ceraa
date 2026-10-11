package coordinator

import (
	"testing"
	"time"

	"ceraa/harness/operation"
)

// A step waits as long as its longest operation may run. The fixed 180s this
// replaced was shorter than the shell default of 15 minutes, so a long command
// was canceled at three minutes and reported as an unknown outcome.
func TestToolTurnWaitOutlastsTheLongestOperation(t *testing.T) {
	prepared := []preparedTool{
		{spec: operation.Spec{Type: "shell", Args: map[string]any{"timeout": 600}}},
		{spec: operation.Spec{Type: "read"}},
	}
	if got, want := toolTurnWait(prepared), 600*time.Second+toolTurnGrace; got != want {
		t.Fatalf("toolTurnWait = %v, want %v", got, want)
	}
}

// The common case sends no timeout argument at all, and the op still runs for the
// shell default, so the wait has to clear the old 180s cut on its own.
func TestToolTurnWaitUsesTheShellDefaultWithoutAnArgument(t *testing.T) {
	wait := toolTurnWait([]preparedTool{{spec: operation.Spec{Type: "shell"}}})
	want := operation.SpecTimeout(operation.Spec{Type: "shell"}) + toolTurnGrace
	if wait != want {
		t.Fatalf("toolTurnWait = %v, want %v", wait, want)
	}
	if wait <= 180*time.Second {
		t.Errorf("toolTurnWait = %v, still at or under the old 180s cut", wait)
	}
}

// An empty batch still yields a positive wait, so a timer is never armed with a
// zero duration (which would fire immediately).
func TestToolTurnWaitIsPositiveForAnEmptyBatch(t *testing.T) {
	if got := toolTurnWait(nil); got <= 0 {
		t.Fatalf("toolTurnWait(nil) = %v, want positive", got)
	}
}
