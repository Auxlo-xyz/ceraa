package instruction

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func repoForTest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"harness/netguard", "prompt"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func compileForTest(t *testing.T, root, body string) Snapshot {
	t.Helper()
	snap, diags := Compile("AGENTS.md", []byte(body))
	if len(diags) > 0 {
		t.Fatalf("the fixture compiled with findings: %s", diags.Error())
	}
	return snap
}

// A rule that names a directory that exists is scoped by its own words, with no
// model and no tag. This is the pass that turns 25 of this repo's own rules from
// dead weight into matchable, which was the measured problem.
func TestARuleNamingARealPathIsScopedByItsOwnWords(t *testing.T) {
	root := repoForTest(t)
	snap := compileForTest(t, root, "- Every network tool goes through harness/netguard for egress.\n")

	res := TagScopes(context.Background(), root, "AGENTS.md", snap, nil)

	if res.Withheld != nil {
		t.Errorf("a rule the filesystem confirmed was withheld: %#v", res.Withheld)
	}
	got := res.Snapshot.Instructions[0]
	if got.Applies == "" {
		t.Fatal("a rule naming a real directory compiled unscoped, so the observer can never match it")
	}
	if got.Applies != "harness/netguard" {
		t.Errorf("applies = %q, want the directory the rule named", got.Applies)
	}
}

// The distinction the whole design turns on. "Never use em dashes" names no path
// because none applies. Withholding these would strip most of a document - on
// this repo's own AGENTS.md the count is 127 of 152.
func TestAGlobalRuleIsLeftAloneRatherThanWithheld(t *testing.T) {
	root := repoForTest(t)
	snap := compileForTest(t, root, "- Never use em dashes in any response.\n- Always use Auxlo XYZ, never Auxlo alone.\n")

	res := TagScopes(context.Background(), root, "AGENTS.md", snap, nil)

	if len(res.Withheld) != 0 {
		t.Errorf("a rule with no path in it was withheld: %#v", res.Withheld)
	}
	for _, rule := range res.Snapshot.Instructions {
		if rule.Applies != "" {
			t.Errorf("%q was given a scope it cannot use", rule.Text)
		}
	}
}

// A path-shaped token that does not resolve is usually not a path at all: a Go
// import, a module path, a shell variable, a home-relative one. Measured, every
// rule in this category on this repo's own file is one of those, so punishing
// them would strip 25 legitimate rules for no stale path among them.
func TestANonResolvingTokenIsNotTreatedAsAFailedScope(t *testing.T) {
	root := repoForTest(t)
	snap := compileForTest(t, root,
		"- Internal imports use ceraa/internal/... for the tree.\n"+
			"- The supervisor derives every path from RUNTIME_DIR.\n")

	res := TagScopes(context.Background(), root, "AGENTS.md", snap, nil)

	if len(res.Withheld) != 0 {
		t.Errorf("a rule mentioning something that is not a filesystem path was withheld: %#v", res.Withheld)
	}
	for _, rule := range res.Snapshot.Instructions {
		if rule.Applies != "" {
			t.Errorf("%q was scoped by a token that resolves to nothing", rule.Text)
		}
	}
}

// A model proposal the filesystem confirms is applied. The model suggests, the
// disk decides - the same discipline the evidence store describes.
func TestAProposalTheFilesystemConfirmsIsApplied(t *testing.T) {
	root := repoForTest(t)
	snap := compileForTest(t, root, "- Keep egress behind the guard in every tool under src/everything.\n")

	proposer := func(context.Context, ProposeRequest) ([]ScopeProposal, error) {
		return []ScopeProposal{{
			RuleID:   snap.Instructions[0].ID,
			Applies:  "harness/netguard/**",
			Triggers: []string{"path"},
			Why:      "the rule is about egress",
		}}, nil
	}

	res := TagScopes(context.Background(), root, "AGENTS.md", snap, proposer)

	if len(res.Withheld) != 0 {
		t.Fatalf("a confirmed proposal was withheld: %#v", res.Withheld)
	}
	got := res.Snapshot.Instructions[0]
	if got.Applies != "harness/netguard/**" {
		t.Errorf("applies = %q, want the confirmed proposal", got.Applies)
	}
	if len(got.Triggers) != 1 || got.Triggers[0] != "path" {
		t.Errorf("triggers = %v, want the proposal's", got.Triggers)
	}
}

// The strict branch, and it fires on a proposal rather than on a rule. A proposal
// that named a path the tree does not have is withheld rather than left unscoped,
// because "unscoped" is indistinguishable from a rule that never wanted a scope
// and would silently revert to once-per-run.
func TestAnUnconfirmableProposalIsWithheldAndNotLeftUnscoped(t *testing.T) {
	root := repoForTest(t)
	snap := compileForTest(t, root, "- Keep egress behind the guard in every tool under src/everything.\n")
	id := snap.Instructions[0].ID

	proposer := func(context.Context, ProposeRequest) ([]ScopeProposal, error) {
		return []ScopeProposal{{RuleID: id, Applies: "harness/search/**", Why: "I think this is where it lives"}}, nil
	}

	res := TagScopes(context.Background(), root, "AGENTS.md", snap, proposer)

	if len(res.Withheld) != 1 {
		t.Fatalf("an unconfirmable proposal was not withheld: %#v", res.Withheld)
	}
	withheld := res.Withheld[0]
	if withheld.Line != res.Snapshot.Instructions[0].Line {
		t.Errorf("withheld line = %d, want the rule's", withheld.Line)
	}
	if withheld.Applies != "harness/search/**" {
		t.Errorf("withheld scope = %q, want the proposal that was refused", withheld.Applies)
	}
	got := res.Snapshot.Instructions[0]
	if got.Applies != withheldApplies {
		t.Errorf("a withheld rule still carries applies = %q, so it can fire", got.Applies)
	}
}

// A rule with no proposal at all is not withheld, because no proposal was made.
// This is what keeps the strict branch narrow: a missing model call is a
// configuration state, not a finding about the rule.
func TestAProposerThatSaysNothingWithholdsNothing(t *testing.T) {
	root := repoForTest(t)
	snap := compileForTest(t, root, "- Keep egress behind the guard in every tool under src/everything.\n")

	res := TagScopes(context.Background(), root, "AGENTS.md", snap, nil)

	if len(res.Withheld) != 0 {
		t.Errorf("no proposer ran, so nothing should be withheld: %#v", res.Withheld)
	}
	if res.Snapshot.Instructions[0].Applies != "" {
		t.Error("a rule the deterministic pass could not scope was scoped anyway")
	}
}

// A proposer that fails must not withhold anything. A failed call is a transient
// condition and the answer to it is to leave the rules as they were, not to
// retire them for a network error.
func TestAFailedProposerLeavesEveryRuleAlone(t *testing.T) {
	root := repoForTest(t)
	snap := compileForTest(t, root, "- Keep egress behind the guard in every tool under src/everything.\n")

	proposer := func(context.Context, ProposeRequest) ([]ScopeProposal, error) {
		return nil, os.ErrDeadlineExceeded
	}

	res := TagScopes(context.Background(), root, "AGENTS.md", snap, proposer)

	if len(res.Withheld) != 0 {
		t.Errorf("a failed proposer withheld rules: %#v", res.Withheld)
	}
	if res.Snapshot.Instructions[0].Applies != "" {
		t.Error("a failed proposer scoped a rule")
	}
}

// A proposal for an id that is not in the file is discarded rather than applied,
// because the id is the only thing tying the pair together and a stray one would
// silently mis-scope an unrelated rule.
func TestAProposalForAnUnknownRuleIsDiscarded(t *testing.T) {
	root := repoForTest(t)
	snap := compileForTest(t, root, "- Keep egress behind the guard in every tool under src/everything.\n")

	proposer := func(context.Context, ProposeRequest) ([]ScopeProposal, error) {
		return []ScopeProposal{{RuleID: "i-does-not-exist", Applies: "harness/netguard"}}, nil
	}

	res := TagScopes(context.Background(), root, "AGENTS.md", snap, proposer)

	if len(res.Withheld) != 0 {
		t.Errorf("an unknown proposal was counted as a withholding: %#v", res.Withheld)
	}
	if res.Snapshot.Instructions[0].Applies != "" {
		t.Error("an unknown proposal was applied to a rule")
	}
}

// The scope a rule gets must not re-identify it. A delivered id has to resolve
// to the same rule next week, which is only true if the id covers the rule's
// words and not where the harness decided to file it.
func TestInferringAScopeDoesNotReIdentifyTheRule(t *testing.T) {
	root := repoForTest(t)
	body := "- Every network tool goes through harness/netguard for egress.\n"

	before := compileForTest(t, root, body)
	after := TagScopes(context.Background(), root, "AGENTS.md", compileForTest(t, root, body), nil)

	if before.Instructions[0].ID != after.Snapshot.Instructions[0].ID {
		t.Errorf("scope inference changed the id: %s then %s",
			before.Instructions[0].ID, after.Snapshot.Instructions[0].ID)
	}
}

// An absolute pattern must keep its leading slash. Without it the token resolves
// against the project root and every absolute path in the file fails to confirm,
// which is silent: the rule just never gets a scope.
func TestAnAbsolutePathKeepsItsLeadingSlash(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if confirmed, ok := resolveScope(root, target); !ok || confirmed != target {
		t.Errorf("resolveScope(%q) = %q, %v; want the absolute path confirmed", target, confirmed, ok)
	}
}

// A glob confirms against its literal prefix, because asking the filesystem about
// "harness/netguard/**" answers about a path that does not exist as such.
func TestAGlobConfirmsAgainstItsLiteralPrefix(t *testing.T) {
	root := repoForTest(t)
	if confirmed, ok := resolveScope(root, "harness/netguard/**"); !ok || confirmed != "harness/netguard/**" {
		t.Errorf("resolveScope(glob) = %q, %v; want the glob confirmed", confirmed, ok)
	}
	if _, ok := resolveScope(root, "harness/search/**"); ok {
		t.Error("a glob whose prefix does not exist was confirmed")
	}
}

// A proposal asking for no scope confirms nothing. That is not a scope, and
// accepting it would be the same as accepting nothing.
func TestAnEmptyProposalConfirmsNothing(t *testing.T) {
	root := repoForTest(t)
	if _, ok := resolveScope(root, "  "); ok {
		t.Error("an empty applies confirmed against the tree")
	}
}
