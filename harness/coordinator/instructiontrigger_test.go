package coordinator

import (
	"path/filepath"
	"reflect"
	"testing"

	"ceraa/harness/instruction"
)

// The trigger is the whole input to selection, so a path that never reaches it
// is a rule that can never fire no matter how correct the rule file is.
func TestAnInstructionTriggerCarriesThePathAnOperationIsAbout(t *testing.T) {
	trigger := InstructionTrigger("read", map[string]any{"path": "api/main.go"}, "read", "/srv", false)

	if trigger.Tool != "read" {
		t.Errorf("the trigger lost the tool name: %+v", trigger)
	}
	want := []string{filepath.Join("/srv", "api", "main.go")}
	if !reflect.DeepEqual(trigger.Targets, want) {
		t.Errorf("targets are %v, want %v", trigger.Targets, want)
	}
	if trigger.Failed {
		t.Error("a successful operation was reported as failed")
	}
}

// The shell form is the normal case rather than the exception: most operations
// carry a command and no path, so this is the branch that has to be right.
func TestAShellCommandTriggerCarriesEveryPathInTheCommand(t *testing.T) {
	trigger := InstructionTrigger("shell", map[string]any{
		"command": "cat b.go a.go c.go",
	}, "shell", "/srv", true)

	want := []string{"/srv/a.go", "/srv/b.go", "/srv/c.go"}
	if !reflect.DeepEqual(trigger.Targets, want) {
		t.Errorf("targets are %v, want %v", trigger.Targets, want)
	}
	if !trigger.Failed {
		t.Error("a failed operation was reported as successful")
	}
}

// An operation about no path at all is the normal case for a web tool, and it is
// the case that must reach Select as an empty target list rather than as nil
// dressed up to look like one. fires() treats them identically, and a trigger
// that carried a placeholder would quietly activate every scope rule instead.
func TestATriggerWithNeitherPathNorCommandHasNoTargets(t *testing.T) {
	trigger := InstructionTrigger("webfetch", map[string]any{"url": "https://example.com"}, "webfetch", "/srv", false)

	if len(trigger.Targets) != 0 {
		t.Errorf("an operation about no path produced targets: %v", trigger.Targets)
	}
	if trigger.Tool != "webfetch" {
		t.Errorf("the trigger lost the tool name: %+v", trigger)
	}
}

// The builder must reach the same answer as the sidekick's own extraction, on
// the cases where that extraction is doing real work rather than returning a
// token.
//
// A second implementation is the specific risk here: the program token of each
// segment, a glob, and a path outside the workdir are three filters that exist
// because containment alone kept the program's own name looking like a file. If
// the trigger builder re-derived paths it would agree with these today and
// disagree the first time one of them was changed, and the disagreement would be
// invisible - an instruction firing on /srv/rm, or on a path the tool was never
// going to touch.
func TestTheTriggerBuilderReusesTheCommandPathWalk(t *testing.T) {
	cases := []struct {
		name    string
		command string
	}{
		{"the program token is not a target", "cat /etc/hosts"},
		{"a glob is a pattern", "rm /srv/*.log"},
		{"a path outside the workdir is dropped", "cat /etc/passwd"},
		{"an extensionless file still counts", "make Makefile"},
		{"a directory is not itself a target", "cd /srv"},
		{"the walk is capped", "cat a.go b.go c.go d.go e.go f.go g.go h.go i.go j.go k.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"command": tc.command}
			walk := targetsForSpec("shell", "/srv", args)
			trigger := InstructionTrigger("shell", args, "shell", "/srv", false)

			if !reflect.DeepEqual(trigger.Targets, walk) {
				t.Errorf("the trigger and the path walk disagree about %q:\n  trigger %v\n  walk %v",
					tc.command, trigger.Targets, walk)
			}
			// And the filter still did its work. Agreement with a broken
			// extraction would satisfy the comparison above while delivering a
			// rule scoped to the program's own name.
			for _, target := range trigger.Targets {
				if target == "/srv/cat" || target == "/srv/rm" || target == "/srv/cd" {
					t.Errorf("%q yielded the program token as a target: %v", tc.command, trigger.Targets)
				}
			}
		})
	}
}

// Two identical operations must produce two identical triggers. Selection is
// compared across runs when a delivery is diagnosed, and a trigger that
// reordered between two calls would make two deliveries of the same rule look
// like two different decisions.
func TestAnInstructionTriggerIsDeterministic(t *testing.T) {
	command := "cat b.go a.go c.go"
	first := InstructionTrigger("shell", map[string]any{"command": command}, "shell", "/srv", false)
	for i := 0; i < 5; i++ {
		got := InstructionTrigger("shell", map[string]any{"command": command}, "shell", "/srv", false)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("the trigger changed between identical calls: %+v then %+v", first, got)
		}
	}
	if first.Targets[0] != "/srv/a.go" {
		t.Errorf("targets are not sorted: %v", first.Targets)
	}
}

// The adapter is a delegation, so this is a wiring pin rather than an algorithm
// test - it would pass on any correct delegation and is here so that a rewrite
// into independent logic cannot land quietly.
//
// The second half is the part worth more: it pins that the delegation is real
// and reaches memory's actual semantics, which are the two cases a naive
// filepath.Match would get wrong. Operations carry absolute paths, so a rule
// written as src/* matches nothing under a naive implementation - a silent
// failure in the most natural way to write one.
func TestTheMatcherAdapterAgreesWithMemoryScope(t *testing.T) {
	cases := []struct {
		applies string
		target  string
	}{
		{"/srv/api", "/srv/api/main.go"},
		{"*.go", "/srv/api/main.go"},
		{"main.go", "/srv/api/main.go"},
		{"src/*", "/srv/app/src/main.go"},
		{"/srv/api", "/srv/api"},
		{"/srv/api", "/srv/apix/main.go"},
		{"", "/srv/main.go"},
		{"/srv/api", ""},
	}
	for _, tc := range cases {
		want := MatchesScope(tc.applies, tc.target)
		if got := instructionMatcher(tc.applies, tc.target); got != want {
			t.Errorf("MatchesScope(%q, %q) = %v, the adapter said %v", tc.applies, tc.target, want, got)
		}
	}

	// The two shapes a hand-rolled matcher gets wrong.
	if !instructionMatcher("src/*", "/srv/app/src/main.go") {
		t.Error("a relative pattern did not reach an absolute target, so the adapter is not memory's matcher")
	}
	if !instructionMatcher("*.go", "/srv/api/main.go") {
		t.Error("a base-name glob did not reach a nested target, so the adapter is not memory's matcher")
	}
}

// The ceiling is documented because it is load-bearing for what anyone should
// build on top of this, and a doc comment nobody reads is not a ceiling. This
// pins the claim the comment makes: the selection itself is a pure function of
// the trigger and the snapshot, and nothing about delivering a note can change
// which calls the model already chose.
func TestAnInstructionTriggerIsTheOnlyThingSelectionGetsToSee(t *testing.T) {
	snapshot := instruction.Snapshot{Instructions: []instruction.Instruction{{
		ID: "id-1", Text: "the backend pins jdk 21", Applies: "/srv/api", Source: "AGENTS.md",
	}}}
	ev := InstructionTrigger("read", map[string]any{"path": "api/main.go"}, "read", "/srv", false)

	first := instruction.Select(snapshot, ev, instruction.NewSeen(), instruction.Options{Matcher: instructionMatcher})
	if len(first.Rules) != 1 {
		t.Fatalf("a scoped rule did not fire: %+v", first)
	}

	// The same trigger after the model has already made its decision yields the
	// same selection, because that decision is not an input to anything here.
	// That is the whole content of the ceiling: this function cannot reach
	// backwards, and a tip attached to a tool result is for the next call.
	after := instruction.Select(snapshot, ev, instruction.NewSeen(), instruction.Options{Matcher: instructionMatcher})
	if !reflect.DeepEqual(first, after) {
		t.Errorf("selection changed for an identical trigger: %+v then %+v", first, after)
	}
}
