package telegram

import (
	"strings"
	"testing"

	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// A registered tool that cannot do anything is the failure this whole feature
// would otherwise repeat: the browser tool answered every call with an error for
// 19 calls before anyone noticed, because nothing checks that a registered tool is
// reachable. So assert the wiring, not just the tool.
func TestSubagentsAreAttachedToTheRegistry(t *testing.T) {
	registry := tool.DefaultRegistryForUser("u", nil)
	task, ok := registry.Get("task")
	if !ok {
		t.Fatal("the task tool is not registered")
	}
	wired, ok := task.(*tool.TaskTool)
	if !ok {
		t.Fatalf("the task tool is %T, want *tool.TaskTool", task)
	}
	if wired.Factory != nil {
		t.Fatal("a fresh registry already has a subagent factory; the tool is meant to be inert until a run attaches one")
	}
	attachSubagents(registry, &subagentFactory{})
	if wired.Factory == nil {
		t.Error("attachSubagents left the task tool without a factory, so every call would fail")
	}
	if wired.MaxConcurrent != 5 {
		t.Errorf("concurrency ceiling is %d, want 5", wired.MaxConcurrent)
	}
}

// A child must not be able to delegate. Translate refuses a nested call, and the
// registry must also not offer the tool, so a crafted call has nothing to reach.
func TestChildRegistryHasNoTaskTool(t *testing.T) {
	registry := tool.DefaultRegistryForUser("u", nil)
	if _, ok := registry.Get("task"); !ok {
		t.Fatal("precondition: the parent should have the task tool")
	}
	if !registry.Remove("task") {
		t.Error("Remove reported the tool was absent")
	}
	if _, ok := registry.Get("task"); ok {
		t.Error("the task tool survived removal, so a child could delegate")
	}
	// Everything else has to survive, or a child is crippled.
	for _, want := range []string{"bash", "read", "write", "edit_line"} {
		if _, ok := registry.Get(want); !ok {
			t.Errorf("removing task also removed %q", want)
		}
	}
}

func TestChildSystemPromptInheritsTheParentAndAddsTheRole(t *testing.T) {
	parent := "PARENT PROMPT MARKER"
	for kind, roleStart := range subagentRolePrompt {
		got := childSystemPrompt(parent, kind)
		if !strings.Contains(got, parent) {
			t.Errorf("%s child lost the parent prompt, so it behaves differently from the agent that spawned it", kind)
		}
		if !strings.Contains(got, roleStart[:20]) {
			t.Errorf("%s child prompt does not carry its role:\n%s", kind, got)
		}
	}
	// An unknown kind falls back rather than producing a child with no role at all.
	if got := childSystemPrompt(parent, "nonsense"); !strings.Contains(got, parent) {
		t.Error("an unknown kind lost the parent prompt")
	}
}

// A budget stop and a crash are different facts, and the parent has to be able to
// tell them apart: a partial report presented as complete is how a model proceeds
// on a wrong belief.
func TestBudgetStopIsDistinguishedFromFailure(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errString("abandoned after 35 requests"), true},
		{errString("soft budget exhausted"), true},
		{errString("provider returned 500"), false},
		{errString("context deadline exceeded"), false},
	}
	for _, tc := range cases {
		if got := isBudgetStop(tc.err); got != tc.want {
			t.Errorf("isBudgetStop(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// The factory's children map was nil at every construction site since the tool
// was written, so the first task call in the daemon's life panicked with
// "assignment to entry in nil map" and took the process down. It survived
// because nothing called NewSubagent: the recorded sessions contain zero task
// calls, and no test reached this seam either.
//
// Built the way production builds it, with no children map, so the guard is
// what is under test rather than the test's own setup.
func TestNewSubagentWorksWithoutAnInitialisedChildrenMap(t *testing.T) {
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &subagentFactory{store: store, parentSession: "parent-1"}
	sub, id, err := f.NewSubagent("general")
	if err != nil {
		t.Fatalf("NewSubagent on a factory with a nil children map: %v", err)
	}
	if sub == nil {
		t.Fatal("NewSubagent returned no subagent")
	}
	if id == "" {
		t.Fatal("NewSubagent returned no id")
	}
	f.mu.Lock()
	child, tracked := f.children[id]
	f.mu.Unlock()
	if !tracked {
		t.Fatalf("child %s is not tracked, so its cost is never rolled into the parent", id)
	}
	if child.kind != "general" {
		t.Errorf("child kind is %q, want general", child.kind)
	}
}
