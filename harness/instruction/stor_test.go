package instruction

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The store must run the scoping pass, not just the compiler. A store that
// cached the compile and skipped TagScopes would look identical while delivering
// nothing matchable.
func TestTheStoreScopesRulesItCompiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "harness/netguard"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"),
		[]byte("- Every network tool goes through harness/netguard for egress.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := NewStore(root, "AGENTS.md")
	snap := store.Refresh()

	if len(snap.Instructions) == 0 {
		t.Fatal("the store compiled nothing")
	}
	if got := snap.Instructions[0].Applies; got != "harness/netguard" {
		t.Errorf("applies = %q, want the directory the rule named", got)
	}
}

// A withheld scope is a finding that survives a cache read, not one that only
// appears on the run that recompiled. Otherwise the finding is read by nobody:
// the file stops changing the moment it is fixed or the moment it is left alone.
func TestAWithheldScopeIsReportedFromTheCacheToo(t *testing.T) {
	root := t.TempDir()
	body := "- Keep egress behind the guard under src/everything.\n"
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var probe ID
	proposer := func(_ context.Context, req ProposeRequest) ([]ScopeProposal, error) {
		for _, rule := range req.Rules {
			probe = rule.ID
			return []ScopeProposal{{RuleID: rule.ID, Applies: "no/such/path", Why: "it seemed right"}}, nil
		}
		return nil, nil
	}

	store := NewStore(root, "AGENTS.md").WithProposer(proposer)
	store.Refresh()

	diags := store.Diagnostics()
	if !diags.Has(DiagUnresolvedScope) {
		t.Fatalf("a refused proposal produced no finding: %s", diags.Error())
	}
	withheldRule := ""
	for _, rule := range store.Snapshot().Instructions {
		if rule.ID == probe {
			withheldRule = rule.Applies
		}
	}
	if withheldRule != withheldApplies {
		t.Errorf("the withheld rule still carries applies = %q, so it can fire", withheldRule)
	}

	// The second refresh takes the cheap path. The finding must survive it, or it
	// is read on exactly one run and by nobody after that.
	store.Refresh()
	if again := store.Diagnostics(); !again.Has(DiagUnresolvedScope) {
		t.Errorf("the withheld-scope finding disappeared on a cache read: %s", again.Error())
	}
}
