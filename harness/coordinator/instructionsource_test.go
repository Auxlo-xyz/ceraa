package coordinator

import (
	"reflect"
	"testing"

	"ceraa/harness/instruction"
)

// oneRule is a snapshot with a single scoped rule, which is the smallest thing
// that can distinguish a working selection from a selection that silently
// returns nothing.
func oneRule() instruction.Snapshot {
	return instruction.Snapshot{
		Source: "AGENTS.md",
		Digest: "d1",
		Instructions: []instruction.Instruction{{
			ID: "id-api", Text: "the backend pins jdk 21",
			Applies: "/srv/api", Source: "AGENTS.md", Line: 12,
			Triggers: []string{"tool:read"},
		}},
	}
}

// The snapshot is loaded once per run and never again. A file read per step is
// the defect this whole change removes, so the counter is the assertion: it
// cannot be satisfied by a test that happens to read a file it also wrote.
func TestTheSnapshotIsLoadedOnceAndReused(t *testing.T) {
	loads := 0
	source := newInstructionSource(func() instruction.Snapshot {
		loads++
		return oneRule()
	}, instructionMatcher)

	for i := 0; i < 5; i++ {
		if got := source.Snapshot(); got.Len() != 1 {
			t.Fatalf("call %d returned %d rules, want 1", i, got.Len())
		}
	}

	if loads != 1 {
		t.Errorf("the rules were compiled %d times in one run, want exactly 1", loads)
	}
	if got := source.Loads(); got != 1 {
		t.Errorf("Loads reports %d, want 1", got)
	}
}

// The same snapshot value comes back every time, not just some snapshot of the
// right size. Select is compared across a run when a delivery is diagnosed, and
// two steps that each saw a different rule set make that comparison meaningless.
func TestTheSnapshotHandedOutIsTheSameOneEveryTime(t *testing.T) {
	source := newInstructionSource(oneRule, instructionMatcher)

	first := source.Snapshot()
	if first.Digest != "d1" {
		t.Fatalf("the loader's snapshot was not used: %+v", first)
	}
	for i := 0; i < 3; i++ {
		if got := source.Snapshot(); !reflect.DeepEqual(got, first) {
			t.Fatalf("call %d returned a different snapshot:\n  %+v\n  %+v", i, got, first)
		}
	}
}

// A project with no AGENTS.md has to be able to stay loaded. Collapsing "empty"
// into "not yet loaded" would recompile the empty file on every step forever,
// which is the per-step read this holder exists to prevent - wearing the
// costume of a project that has written no rules.
func TestAnEmptySnapshotStillCountsAsLoaded(t *testing.T) {
	loads := 0
	source := newInstructionSource(func() instruction.Snapshot {
		loads++
		return instruction.Snapshot{}
	}, instructionMatcher)

	for i := 0; i < 3; i++ {
		if got := source.Snapshot(); got.Len() != 0 {
			t.Fatalf("call %d returned %d rules from an empty source", i, got.Len())
		}
	}
	if loads != 1 {
		t.Errorf("an empty snapshot was recompiled %d times, want 1", loads)
	}
}

// One run shares one delivered set. The dedup it exists for is the whole reason
// a scoped rule does not flood the transcript, and a fresh set per call would
// silently retire it - every call would look like the first.
func TestTheSourceHandsOutOneSeenSetForTheRun(t *testing.T) {
	source := newInstructionSource(oneRule, instructionMatcher)

	first := source.Seen()
	if first == nil {
		t.Fatal("the source handed out no delivered set")
	}
	if second := source.Seen(); second != first {
		t.Error("the source handed out a different delivered set on the second call")
	}

	// The shared set is the working one, not just the same pointer.
	first.Mark("id-api")
	if !source.Seen().Delivered("id-api") {
		t.Error("a rule marked through one handle is not visible through the next")
	}
}

// The matcher is never nil after construction. A nil Matcher is meaningful to
// instruction.Select - it withholds every scoped rule - but that answers "no
// matcher is configured", and in this binary one is configured. Defaulting is
// what stops an ordinary construction from withholding every scope rule and
// looking like a project that wrote none.
func TestTheSourceNeverHandsOutANilMatcher(t *testing.T) {
	withNil := newInstructionSource(oneRule, nil)
	if withNil.Matcher() == nil {
		t.Fatal("a source built with no matcher handed out nil, so every scoped rule would be withheld")
	}

	// And the matcher it defaulted to is the real one, not a stub.
	if !withNil.Matcher()("/srv/api", "/srv/api/main.go") {
		t.Error("the default matcher does not match a directory scope against a file beneath it")
	}
	if withNil.Matcher()("/srv/api", "/srv/web/main.go") {
		t.Error("the default matcher matched a scope that does not cover the target")
	}
}

// End to end: a rule whose scope covers the operation's target is delivered
// through the holder. This is the shape the runner will use, and it is the only
// test here that would notice a snapshot, a seen set and a matcher that all
// exist but do not reach each other.
func TestTheSourceSelectsARuleForAnOperationItCovers(t *testing.T) {
	source := newInstructionSource(oneRule, instructionMatcher)

	onTarget := InstructionTrigger("read", map[string]any{"path": "api/main.go"}, "read", "/srv", false)
	got := source.Select(onTarget)
	if len(got.Rules) != 1 {
		t.Fatalf("a rule scoped to the operation's path did not fire: %+v", got)
	}
	if got.Matched != 1 || got.Withheld != 0 {
		t.Errorf("the selection misreported itself: %+v", got)
	}

	offTarget := InstructionTrigger("read", map[string]any{"path": "web/main.go"}, "read", "/srv", false)
	if got := source.Select(offTarget); len(got.Rules) != 0 {
		t.Errorf("a rule fired for a path outside its scope: %+v", got)
	}
}

// The trigger's tool has to be able to fire a rule on its own, with no path
// involved. This is the "when a build fails" case, and it is the one shape the
// package could get right for paths while silently never delivering anything on
// an error.
func TestTheSourceFiresAToolAndErrorTrigger(t *testing.T) {
	source := newInstructionSource(func() instruction.Snapshot {
		snap := oneRule()
		snap.Instructions[0].Triggers = []string{"error"}
		snap.Instructions[0].Applies = ""
		return snap
	}, instructionMatcher)

	failed := InstructionTrigger("shell", map[string]any{"command": "go build ./..."}, "shell", "/srv", true)
	if got := source.Select(failed); len(got.Rules) != 1 {
		t.Errorf("an error trigger did not fire on a failed operation: %+v", got)
	}

	succeeded := InstructionTrigger("shell", map[string]any{"command": "go build ./..."}, "shell", "/srv", false)
	if got := source.Select(succeeded); len(got.Rules) != 0 {
		t.Errorf("an error trigger fired on an operation that succeeded: %+v", got)
	}
}

// Select must not mark anything delivered. The order is policy - mark only after
// the note survives its own byte ceiling, or a rule gets retired having never
// been seen - and a Select that marked would make that unfixable downstream
// without editing the selection engine.
func TestSelectingDoesNotMarkAnythingDelivered(t *testing.T) {
	source := newInstructionSource(oneRule, instructionMatcher)
	ev := InstructionTrigger("read", map[string]any{"path": "api/main.go"}, "read", "/srv", false)

	if got := source.Select(ev); len(got.Rules) != 1 {
		t.Fatalf("nothing was selected to mark: %+v", got)
	}
	if got := source.Seen().Len(); got != 0 {
		t.Errorf("selection marked %d rules delivered, so whoever renders the note has no say in it", got)
	}
}

// A nil source is a state, not a crash. The source is built on a run path other
// agents own, and a run with no instruction store configured must still run.
func TestANilSourceAnswersRatherThanPanicking(t *testing.T) {
	var source *instructionSource

	if got := source.Snapshot(); got.Len() != 0 {
		t.Errorf("a nil source returned %d rules", got.Len())
	}
	if source.Seen() == nil {
		t.Error("a nil source handed out no delivered set")
	}
	if source.Matcher() == nil {
		t.Error("a nil source handed out no matcher")
	}
	if source.Loads() != 0 {
		t.Errorf("a nil source reports %d loads", source.Loads())
	}
	if got := source.Select(InstructionTrigger("read", nil, "read", "/srv", false)); len(got.Rules) != 0 {
		t.Errorf("a nil source selected %d rules", len(got.Rules))
	}
}
