package verify

// verify_test.go pins the policy: the verdict rule, the totality of the
// decision, and the promise that a control which misbehaves cannot take the
// process down.

import (
	"reflect"
	"strings"
	"testing"
)

// fixedControl is a control that resolves to one state without looking at
// the observation, so a table row can name the state it is about.
func fixedControl(name string, state ObligationState) Control {
	return Control{
		Name:     name,
		Question: "the control resolves to a fixed state",
		Enforce: func(o Observation) Obligation {
			return Obligation{State: state, Evidence: "the fixed state"}
		},
	}
}

// baselineExchange is the shape the controls expect: a request with a
// payload, a response that carries it back, and a baseline that does not.
func baselineExchange() Observation {
	return Observation{
		URL:          "https://example.com/search?q=ceraa-marker-1234",
		Method:       "GET",
		Status:       200,
		ResponseBody: "you searched for ceraa-marker-1234 and found 3 records",
		Baseline: &Observation{
			URL:          "https://example.com/search?q=",
			Method:       "GET",
			Status:       200,
			ResponseBody: "you searched for  and found 3 records",
		},
	}
}

// executionExchange is the same request answered differently: the payload
// was executed rather than printed back.
func executionExchange() Observation {
	return Observation{
		URL:          "https://example.com/search?q=ceraa-marker-1234",
		Method:       "GET",
		Status:       500,
		ResponseBody: "ceraa-marker-1234 was executed by the index: 3 internal records",
		Baseline: &Observation{
			URL:          "https://example.com/search?q=",
			Method:       "GET",
			Status:       200,
			ResponseBody: "nothing was executed",
		},
	}
}

// TestTheVerdictRule walks every mixture of obligations and names the
// verdict each one has to produce.
func TestTheVerdictRule(t *testing.T) {
	tests := []struct {
		name     string
		controls []Control
		want     Verdict
	}{
		{name: "no control at all", controls: nil, want: Unproven},
		{name: "one established", controls: []Control{fixedControl("a", Established)}, want: Proven},
		{name: "two established", controls: []Control{fixedControl("a", Established), fixedControl("b", Established)}, want: Proven},
		{name: "one contradicted", controls: []Control{fixedControl("a", Contradicted)}, want: Refuted},
		{
			name:     "a contradiction beside an established obligation",
			controls: []Control{fixedControl("a", Established), fixedControl("b", Contradicted)},
			want:     Refuted,
		},
		{
			name:     "a contradiction beside a missing obligation",
			controls: []Control{fixedControl("a", Missing), fixedControl("b", Contradicted)},
			want:     Refuted,
		},
		{
			name:     "a missing obligation beside an established one",
			controls: []Control{fixedControl("a", Established), fixedControl("b", Missing)},
			want:     Unproven,
		},
		{
			name:     "three established and one missing",
			controls: []Control{fixedControl("a", Established), fixedControl("b", Established), fixedControl("c", Established), fixedControl("d", Missing)},
			want:     Unproven,
		},
		{
			name:     "nothing could be resolved",
			controls: []Control{fixedControl("a", Missing), fixedControl("b", Missing)},
			want:     Inconclusive,
		},
		{
			name:     "one obligation that could not be resolved",
			controls: []Control{fixedControl("a", Missing)},
			want:     Inconclusive,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := Decide(baselineExchange(), test.controls)
			if decision.Verdict != test.want {
				t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, test.want, decision.Obligations)
			}
			if len(decision.Reasons) == 0 {
				t.Fatalf("the decision carries no reason for the verdict %s", decision.Verdict)
			}
		})
	}
}

// TestEveryVerdictIsReachable walks the mixtures again and refuses a policy
// that leaves one of the four verdicts unreachable.
func TestEveryVerdictIsReachable(t *testing.T) {
	mixes := [][]ObligationState{
		{Established},
		{Contradicted},
		{Missing},
		{Established, Missing},
	}
	seen := map[Verdict]bool{}
	for _, mix := range mixes {
		controls := make([]Control, 0, len(mix))
		for i, state := range mix {
			controls = append(controls, fixedControl(string(rune('a'+i)), state))
		}
		seen[Decide(baselineExchange(), controls).Verdict] = true
	}
	for _, verdict := range []Verdict{Proven, Refuted, Unproven, Inconclusive} {
		if !seen[verdict] {
			t.Errorf("the verdict %s is unreachable", verdict)
		}
	}
}

// TestAnEmptyControlListIsUnproven pins the answer for an exchange nobody
// asked anything about.
func TestAnEmptyControlListIsUnproven(t *testing.T) {
	decision := Decide(baselineExchange(), nil)
	if decision.Verdict != Unproven {
		t.Fatalf("verdict is %s, want %s", decision.Verdict, Unproven)
	}
	if len(decision.Obligations) != 0 {
		t.Fatalf("the decision carries %d obligations, want none", len(decision.Obligations))
	}
	if len(decision.Reasons) == 0 {
		t.Fatal("the decision carries no reason")
	}
}

// TestAControlThatPanicsBecomesAMissingObligation pins the promise that a
// misbehaving control cannot take the process down and cannot pass itself.
func TestAControlThatPanicsBecomesAMissingObligation(t *testing.T) {
	control := Control{
		Name:     "panicking",
		Question: "the control never answers",
		Enforce: func(o Observation) Obligation {
			panic("the control fell over")
		},
	}

	decision := Decide(baselineExchange(), []Control{control})
	if decision.Verdict == Proven {
		t.Fatal("a panicking control proved the finding")
	}
	if decision.Verdict != Inconclusive {
		t.Fatalf("verdict is %s, want %s", decision.Verdict, Inconclusive)
	}
	if len(decision.Obligations) != 1 {
		t.Fatalf("the decision carries %d obligations, want one", len(decision.Obligations))
	}
	obligation := decision.Obligations[0]
	if obligation.State != Missing {
		t.Fatalf("the obligation is %s, want %s", obligation.State, Missing)
	}
	if !strings.Contains(obligation.Evidence, "the control fell over") {
		t.Fatalf("the obligation does not name the panic: %q", obligation.Evidence)
	}
}

// TestAControlThatReturnsNothingIsAMissingObligation pins the answer for a
// control that returns the zero obligation: a state outside the vocabulary
// has not answered the question either.
func TestAControlThatReturnsNothingIsAMissingObligation(t *testing.T) {
	control := Control{
		Name:     "silent",
		Question: "the control returns nothing",
		Enforce: func(o Observation) Obligation {
			return Obligation{}
		},
	}

	decision := Decide(baselineExchange(), []Control{control})
	if decision.Verdict != Inconclusive {
		t.Fatalf("verdict is %s, want %s", decision.Verdict, Inconclusive)
	}
	if decision.Obligations[0].State != Missing {
		t.Fatalf("the obligation is %s, want %s", decision.Obligations[0].State, Missing)
	}
}

// TestAControlWithoutAnEnforcementIsAMissingObligation pins the answer for a
// control that was never wired up.
func TestAControlWithoutAnEnforcementIsAMissingObligation(t *testing.T) {
	decision := Decide(baselineExchange(), []Control{{Name: "empty", Question: "nothing was wired up"}})
	if decision.Verdict != Inconclusive {
		t.Fatalf("verdict is %s, want %s", decision.Verdict, Inconclusive)
	}
	if decision.Obligations[0].State != Missing {
		t.Fatalf("the obligation is %s, want %s", decision.Obligations[0].State, Missing)
	}
}

// TestInconclusiveIsReachableFromEveryUnrunPath walks the ways a check can
// fail to run and insists each of them lands on the verdict that says so.
func TestInconclusiveIsReachableFromEveryUnrunPath(t *testing.T) {
	marker := "ceraa-marker-1234"

	tests := []struct {
		name     string
		controls []Control
		exchange Observation
	}{
		{
			name:     "the baseline the marker had to be anchored against is missing",
			controls: []Control{MarkerControl(marker)},
			exchange: Observation{ResponseBody: "you searched for " + marker},
		},
		{
			name:     "the response carries no body to look at",
			controls: []Control{MarkerControl(marker)},
			exchange: Observation{Baseline: &Observation{ResponseBody: "nothing"}},
		},
		{
			name:     "the control returned a missing obligation",
			controls: []Control{fixedControl("missing", Missing)},
			exchange: baselineExchange(),
		},
		{
			name:     "the control returned the zero obligation",
			controls: []Control{fixedControl("silent", "")},
			exchange: baselineExchange(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := Decide(test.exchange, test.controls)
			if decision.Verdict != Inconclusive {
				t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, Inconclusive, decision.Obligations)
			}
		})
	}
}

// TestAnEmptyExchangeIsInconclusive pins the answer for an exchange that
// carries nothing at all.
func TestAnEmptyExchangeIsInconclusive(t *testing.T) {
	decision := Decide(Observation{}, DefaultControls())
	if decision.Verdict != Inconclusive {
		t.Fatalf("verdict is %s, want %s", decision.Verdict, Inconclusive)
	}
	for _, obligation := range decision.Obligations {
		if obligation.State != Missing {
			t.Fatalf("the obligation %q is %s, want %s", obligation.Name, obligation.State, Missing)
		}
	}
}

// TestTheDecisionCarriesOneObligationPerControl pins the shape of the
// record: a reader has to be able to line an obligation up with the control
// that produced it.
func TestTheDecisionCarriesOneObligationPerControl(t *testing.T) {
	controls := []Control{fixedControl("a", Established), fixedControl("b", Contradicted)}

	decision := Decide(baselineExchange(), controls)
	if len(decision.Obligations) != len(controls) {
		t.Fatalf("the decision carries %d obligations, want %d", len(decision.Obligations), len(controls))
	}
	for i, control := range controls {
		obligation := decision.Obligations[i]
		if obligation.Name != control.Name {
			t.Fatalf("obligation %d is named %q, want %q", i, obligation.Name, control.Name)
		}
		if obligation.Detail != control.Question {
			t.Fatalf("obligation %d carries the detail %q, want the question %q", i, obligation.Detail, control.Question)
		}
	}
}

// TestAControlKeepsTheDetailItSets pins the rule that a control may state
// its own obligation in its own words.
func TestAControlKeepsTheDetailItSets(t *testing.T) {
	control := Control{
		Name:     "worded",
		Question: "the question the control was built with",
		Enforce: func(o Observation) Obligation {
			return Obligation{Detail: "the obligation in the control's own words", State: Established}
		},
	}

	decision := Decide(baselineExchange(), []Control{control})
	if decision.Obligations[0].Detail != "the obligation in the control's own words" {
		t.Fatalf("the obligation carries the detail %q, want the one the control set", decision.Obligations[0].Detail)
	}
}

// TestTheReasonsNameTheUnmetObligation pins the promise that a reader can
// disagree with a result: the unmet obligation is named with what was
// observed.
func TestTheReasonsNameTheUnmetObligation(t *testing.T) {
	control := Control{
		Name:     "named",
		Question: "the question the control was built with",
		Enforce: func(o Observation) Obligation {
			return Obligation{State: Contradicted, Evidence: "the response was identical to the baseline"}
		},
	}

	decision := Decide(baselineExchange(), []Control{control})
	joined := strings.Join(decision.Reasons, "\n")
	if !strings.Contains(joined, "named") {
		t.Fatalf("the reasons do not name the unmet obligation: %q", joined)
	}
	if !strings.Contains(joined, "the response was identical to the baseline") {
		t.Fatalf("the reasons do not carry what was observed: %q", joined)
	}
}

// TestDecideIsPureAndTotal pins the promise that the policy reads its input
// and answers, and touches nothing else.
func TestDecideIsPureAndTotal(t *testing.T) {
	exchange := baselineExchange()
	before := baselineExchange()
	controls := DefaultControls()

	first := Decide(exchange, controls)
	second := Decide(exchange, controls)

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two decisions over the same exchange differ:\n%v\n%v", first, second)
	}
	if !reflect.DeepEqual(before, exchange) {
		t.Fatal("the policy changed the observation it was given")
	}
	if len(first.Obligations) != len(controls) {
		t.Fatalf("the decision carries %d obligations, want %d", len(first.Obligations), len(controls))
	}
}
