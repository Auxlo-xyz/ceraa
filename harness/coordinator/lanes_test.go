package coordinator

import (
	"testing"

	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// A batch is dispatched per call now, each onto its own lane, so a read beside
// an edit is not held behind the edit. The batch-wide flag is what used to
// serialize the reads, and this is the test that fails if it comes back.
func TestABatchRendersEachCallToItsOwnLane(t *testing.T) {
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{reg: tool.DefaultRegistry(), store: store, opMgr: operation.New(nil), cfg: RunnerConfig{SessionID: "lanes"}}

	lanes := map[string]string{}
	for _, name := range []string{"read", "write", "browser_click", "run_command_with_secret"} {
		reg, ok := runner.reg.Get(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		lanes[name] = sequentialQueueFor(reg)
	}

	// Three of these contend for the filesystem and must not be allowed to
	// interleave; the browser does not touch files, and the secret command and
	// the media send are their own concerns.
	for _, pair := range [][2]string{
		{"write", "edit_line"},
		{"edit_line", "batch_edit_file"},
	} {
		reg, _ := runner.reg.Get(pair[0])
		reg2, _ := runner.reg.Get(pair[1])
		if sequentialQueueFor(reg) != sequentialQueueFor(reg2) {
			t.Errorf("%s and %s are on different lanes (%q, %q): they both write files",
				pair[0], pair[1], sequentialQueueFor(reg), sequentialQueueFor(reg2))
		}
	}
	if lanes["browser_click"] == lanes["write"] {
		t.Error("a browser action shares the filesystem lane")
	}
	if lanes["read"] != "" && lanes["read"] == lanes["write"] {
		t.Error("a read shares the filesystem lane")
	}
}

// A tool that does not name a lane goes on the default one, which serialises
// against everything else on it. That is the conservative behaviour for a tool
// whose contention is unknown.
func TestAnUnnamedSequentialToolTakesTheDefaultLane(t *testing.T) {
	if got := sequentialQueueFor(tool.Tool(&tool.GetGoalTool{})); got != operation.DefaultSequentialQueue {
		t.Errorf("a non-sequential tool resolved to %q", got)
	}
}
