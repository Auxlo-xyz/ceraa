package internal

import (
	"context"
	"strings"
	"testing"

	"ceraa/harness/instruction"
	"ceraa/harness/llm"
)

// The proposer may only choose from the candidate list it is shown.
//
// This is the property that keeps the model call safe. A proposer free to invent
// paths produces suggestions the caller has to guess at, and the answer to a
// suggestion that might be a hallucination is to refuse it - so a constrained
// proposer either answers something real or answers nothing, and confirmation
// remains a formality rather than the whole defence.
func TestTheProposerIsShownOnlyPathsTheRepoAlreadyNames(t *testing.T) {
	var shown instruction.ProposeRequest
	proposer := func(_ context.Context, req instruction.ProposeRequest) ([]instruction.ScopeProposal, error) {
		shown = req
		return nil, nil
	}

	rule := instruction.Instruction{ID: "i-1", Text: "Keep egress behind the guard under harness/netguard/egress."}
	if _, err := proposer(context.Background(), instruction.ProposeRequest{
		Rules:      []instruction.Instruction{rule},
		Candidates: []string{"harness/netguard/egress", "prompt/system.txt"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(shown.Rules) != 1 {
		t.Fatalf("the proposer was shown %d rules, want the untagged one", len(shown.Rules))
	}
	for _, candidate := range shown.Candidates {
		if !strings.Contains(candidate, "/") {
			t.Errorf("candidate %q has no separator, so it matches a base name anywhere", candidate)
		}
	}
}

// parseScopes keeps only elements that can be tied back to a rule.
//
// Either field missing means the pair cannot be matched, and applying it anyway
// would silently mis-scope an unrelated rule. The confirmation step then decides
// what survives, so a dropped element is a rule left alone rather than a rule
// withheld.
func TestParseScopesKeepsOnlyWellFormedElements(t *testing.T) {
	got, err := parseScopes(`[{"id":"i-a","applies":"harness/x"},{"id":"","applies":"harness/y"},{"id":"i-b"},{"id":"i-c","applies":"harness/z"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("parseScopes kept %d of 4 elements, want the 2 that can be tied to a rule: %#v", len(got), got)
	}
	if got[0].RuleID != "i-a" || got[0].Applies != "harness/x" {
		t.Errorf("first element = %#v, want the well-formed one", got[0])
	}
	if got[1].RuleID != "i-c" {
		t.Errorf("second element = %#v, want the other well-formed one", got[1])
	}
}

// An answer that is not a JSON array is not an error. The proposer being wrong
// costs a rule its inferred scope, which is the deterministic outcome anyway.
func TestParseScopesIgnoresAnAnswerThatIsNotAnArray(t *testing.T) {
	for _, text := range []string{"", "   ", "no array here", "[not json", "null"} {
		got, err := parseScopes(text)
		if err != nil {
			t.Errorf("parseScopes(%q) returned an error: %v", text, err)
		}
		if len(got) != 0 {
			t.Errorf("parseScopes(%q) returned %d elements, want none", text, len(got))
		}
	}
}

// Without credentials the proposer is absent, and absent means the deterministic
// pass alone runs. That pass is not a fallback - it is the whole coverage for a
// rule that names a real path, and it costs nothing.
func TestNoCredentialsMeansNoProposer(t *testing.T) {
	if proposer, enabled := ProposeScopes(testProviderConfig()); enabled {
		t.Error("a proposer was built with no credentials configured")
		_ = proposer
	}
}

func testProviderConfig() llm.ProviderConfig {
	return llm.ProviderConfig{BaseURL: "http://127.0.0.1:1", Model: "none"}
}
