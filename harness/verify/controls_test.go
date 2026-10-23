package verify

// controls_test.go pins the controls: the marker guard that refuses a canary
// the baseline already carried, and the three controls that tell a payload
// that was executed from one that was only printed back.

import (
	"reflect"
	"strings"
	"testing"
)

// TestTheMarkerGuardAcceptsAMarkerTheBaselineDidNotCarry pins the happy
// path of the guard: the canary came back and the baseline did not carry it.
func TestTheMarkerGuardAcceptsAMarkerTheBaselineDidNotCarry(t *testing.T) {
	exchange := Observation{
		ResponseBody: "you searched for ceraa-marker-1234 and found 3 records",
		Baseline:     &Observation{ResponseBody: "you searched for  and found 3 records"},
	}

	decision := Decide(exchange, []Control{MarkerControl("ceraa-marker-1234")})
	if decision.Verdict != Proven {
		t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, Proven, decision.Obligations)
	}
}

// TestTheMarkerGuardRefusesAMarkerTheBaselineCarries pins the false-Proven
// guard: a marker the baseline carried as well proves only that the server
// echoes input.
func TestTheMarkerGuardRefusesAMarkerTheBaselineCarries(t *testing.T) {
	exchange := Observation{
		ResponseBody: "you searched for ceraa-marker-1234 and found 3 records",
		Baseline:     &Observation{ResponseBody: "you searched for ceraa-marker-1234 and found 3 records"},
	}

	decision := Decide(exchange, []Control{MarkerControl("ceraa-marker-1234")})
	if decision.Verdict != Refuted {
		t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, Refuted, decision.Obligations)
	}
	obligation := decision.Obligations[0]
	if obligation.State != Contradicted {
		t.Fatalf("the obligation is %s, want %s", obligation.State, Contradicted)
	}
	if !strings.Contains(obligation.Evidence, "baseline") {
		t.Fatalf("the obligation does not name the baseline: %q", obligation.Evidence)
	}
}

// TestTheMarkerGuardWithoutABaselineIsMissing pins the rule that a check
// that could not run is not a check that passed.
func TestTheMarkerGuardWithoutABaselineIsMissing(t *testing.T) {
	exchange := Observation{ResponseBody: "you searched for ceraa-marker-1234 and found 3 records"}

	decision := Decide(exchange, []Control{MarkerControl("ceraa-marker-1234")})
	if decision.Verdict != Inconclusive {
		t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, Inconclusive, decision.Obligations)
	}
	obligation := decision.Obligations[0]
	if obligation.State != Missing {
		t.Fatalf("the obligation is %s, want %s", obligation.State, Missing)
	}
	if !strings.Contains(obligation.Evidence, "baseline") {
		t.Fatalf("the obligation does not name what was missing: %q", obligation.Evidence)
	}
}

// TestTheMarkerGuardRefusesAMarkerTheResponseDoesNotCarry pins the ordinary
// refutation: the payload did not come back at all.
func TestTheMarkerGuardRefusesAMarkerTheResponseDoesNotCarry(t *testing.T) {
	exchange := Observation{
		ResponseBody: "you searched for nothing and found 3 records",
		Baseline:     &Observation{ResponseBody: "you searched for  and found 3 records"},
	}

	decision := Decide(exchange, []Control{MarkerControl("ceraa-marker-1234")})
	if decision.Verdict != Refuted {
		t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, Refuted, decision.Obligations)
	}
}

// TestMarkerInBaseline pins the helper the guard is built on.
func TestMarkerInBaseline(t *testing.T) {
	baseline := &Observation{ResponseBody: "you searched for ceraa-marker-1234 and found 3 records"}

	if !markerInBaseline("ceraa-marker-1234", baseline) {
		t.Fatal("the baseline carries the marker and the helper says it does not")
	}
	if markerInBaseline("ceraa-marker-5678", baseline) {
		t.Fatal("the baseline does not carry the marker and the helper says it does")
	}
	if markerInBaseline("ceraa-marker-1234", nil) {
		t.Fatal("there is no baseline and the helper claims the marker is in it")
	}
	if markerInBaseline("", baseline) {
		t.Fatal("there is no marker and the helper claims one is in the baseline")
	}
}

// TestTheMarkerGuardFindsACanaryInTheRequest pins the discovered form of the
// guard: the canary is whatever the request carried.
func TestTheMarkerGuardFindsACanaryInTheRequest(t *testing.T) {
	exchange := Observation{
		URL:          "https://example.com/search?q=ceraa-marker-1234",
		ResponseBody: "you searched for ceraa-marker-1234 and found 3 records",
		Baseline:     &Observation{ResponseBody: "you searched for  and found 3 records"},
	}

	decision := Decide(exchange, []Control{MarkerGuardControl()})
	if decision.Verdict != Proven {
		t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, Proven, decision.Obligations)
	}
}

// TestTheMarkerGuardFindsACanaryInARequestBody pins the discovered form of
// the guard for a request that carries its payload in the body.
func TestTheMarkerGuardFindsACanaryInARequestBody(t *testing.T) {
	exchange := Observation{
		URL:          "https://example.com/api",
		Method:       "POST",
		RequestBody:  `{"query":"ceraa-marker-1234"}`,
		ResponseBody: "the index answered for ceraa-marker-1234",
		Baseline:     &Observation{ResponseBody: "the index answered for nothing"},
	}

	decision := Decide(exchange, []Control{MarkerGuardControl()})
	if decision.Verdict != Proven {
		t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, Proven, decision.Obligations)
	}
}

// TestTheStatusControl pins the control that tells a status a public page
// returns from a status the finding expects.
func TestTheStatusControl(t *testing.T) {
	tests := []struct {
		name     string
		exchange Observation
		control  Control
		want     Verdict
	}{
		{
			name:     "the status differs from the baseline",
			exchange: Observation{Status: 500, Baseline: &Observation{Status: 200}},
			control:  StatusControl(),
			want:     Proven,
		},
		{
			name:     "the status is the one the baseline returned",
			exchange: Observation{Status: 200, Baseline: &Observation{Status: 200}},
			control:  StatusControl(),
			want:     Refuted,
		},
		{
			name:     "there is no baseline to compare against",
			exchange: Observation{Status: 500},
			control:  StatusControl(),
			want:     Inconclusive,
		},
		{
			name:     "the exchange carries no status",
			exchange: Observation{Baseline: &Observation{Status: 200}},
			control:  StatusControl(),
			want:     Inconclusive,
		},
		{
			name:     "the status is the one the finding expects",
			exchange: Observation{Status: 500, Baseline: &Observation{Status: 500}},
			control:  StatusControl(500),
			want:     Proven,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := Decide(test.exchange, []Control{test.control})
			if decision.Verdict != test.want {
				t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, test.want, decision.Obligations)
			}
		})
	}
}

// TestTheBodyChangeControl pins the control that an unchanged body is the
// clearest possible refutation.
func TestTheBodyChangeControl(t *testing.T) {
	tests := []struct {
		name     string
		exchange Observation
		want     Verdict
	}{
		{
			name:     "the body differs from the baseline",
			exchange: Observation{ResponseBody: "new", Baseline: &Observation{ResponseBody: "old"}},
			want:     Proven,
		},
		{
			name:     "the body is the one the baseline returned",
			exchange: Observation{ResponseBody: "same", Baseline: &Observation{ResponseBody: "same"}},
			want:     Refuted,
		},
		{
			name:     "there is no baseline to compare against",
			exchange: Observation{ResponseBody: "new"},
			want:     Inconclusive,
		},
		{
			name:     "neither the response nor the baseline carries a body",
			exchange: Observation{},
			want:     Inconclusive,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := Decide(test.exchange, []Control{BodyChangeControl()})
			if decision.Verdict != test.want {
				t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, test.want, decision.Obligations)
			}
		})
	}
}

// TestTheReflectionControl pins the control that tells a payload that was
// executed from one that was only printed back.
func TestTheReflectionControl(t *testing.T) {
	tests := []struct {
		name     string
		exchange Observation
		want     Verdict
	}{
		{
			name: "the response differs by nothing but the echoed payload",
			exchange: Observation{
				URL:          "https://example.com/search?q=ceraa-marker-1234",
				ResponseBody: "you searched for ceraa-marker-1234 and found 3 records",
				Baseline:     &Observation{ResponseBody: "you searched for  and found 3 records"},
			},
			want: Refuted,
		},
		{
			name: "the response differs by more than the echoed payload",
			exchange: Observation{
				URL:          "https://example.com/search?q=ceraa-marker-1234",
				ResponseBody: "ceraa-marker-1234 was executed: 3 internal records",
				Baseline:     &Observation{ResponseBody: "nothing was executed"},
			},
			want: Proven,
		},
		{
			name: "no value the request carried came back",
			exchange: Observation{
				URL:          "https://example.com/search?q=ceraa-marker-1234",
				ResponseBody: "you searched for nothing and found 3 records",
				Baseline:     &Observation{ResponseBody: "you searched for  and found 3 records"},
			},
			want: Refuted,
		},
		{
			name: "the request carried no payload",
			exchange: Observation{
				URL:          "https://example.com/search",
				ResponseBody: "you searched for nothing and found 3 records",
				Baseline:     &Observation{ResponseBody: "you searched for nothing and found 3 records"},
			},
			want: Inconclusive,
		},
		{
			name: "there is no baseline to compare against",
			exchange: Observation{
				URL:          "https://example.com/search?q=ceraa-marker-1234",
				ResponseBody: "you searched for ceraa-marker-1234 and found 3 records",
			},
			want: Inconclusive,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := Decide(test.exchange, []Control{ReflectionControl()})
			if decision.Verdict != test.want {
				t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, test.want, decision.Obligations)
			}
		})
	}
}

// TestTheSecretControl pins the control that a credential-shaped value in a
// response is a finding and that the value is masked when it is reported.
func TestTheSecretControl(t *testing.T) {
	tests := []struct {
		name     string
		exchange Observation
		want     Verdict
	}{
		{
			name:     "the body carries a credential",
			exchange: Observation{ResponseBody: "AKIA1234567890ABCDEF"},
			want:     Proven,
		},
		{
			name:     "a header carries a credential",
			exchange: Observation{ResponseHeaders: map[string]string{"x-debug-key": "AKIA1234567890ABCDEF"}},
			want:     Proven,
		},
		{
			name:     "the response carries no credential",
			exchange: Observation{ResponseBody: "the page you asked for"},
			want:     Refuted,
		},
		{
			name:     "the response carries nothing to scan",
			exchange: Observation{},
			want:     Inconclusive,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := Decide(test.exchange, []Control{SecretControl()})
			if decision.Verdict != test.want {
				t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, test.want, decision.Obligations)
			}
		})
	}
}

// TestDefaultControlsCoverTheCommonWebFindings pins the set the package
// offers and the names that identify it in a report.
func TestDefaultControlsCoverTheCommonWebFindings(t *testing.T) {
	controls := DefaultControls()

	want := []string{"marker", "status", "body", "reflection", "secret"}
	if len(controls) != len(want) {
		t.Fatalf("the default set carries %d controls, want %d", len(controls), len(want))
	}
	for i, control := range controls {
		if control.Name != want[i] {
			t.Fatalf("control %d is named %q, want %q", i, control.Name, want[i])
		}
		if strings.TrimSpace(control.Question) == "" {
			t.Fatalf("the control %q carries no question", control.Name)
		}
		if control.Enforce == nil {
			t.Fatalf("the control %q enforces nothing", control.Name)
		}
	}
}

// TestAPayloadThatWasExecutedIsProven pins the shape a caller uses: the
// controls that match the finding, and a verdict.
func TestAPayloadThatWasExecutedIsProven(t *testing.T) {
	controls := []Control{
		MarkerGuardControl(),
		StatusControl(),
		BodyChangeControl(),
		ReflectionControl(),
	}

	decision := Decide(executionExchange(), controls)
	if decision.Verdict != Proven {
		t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, Proven, decision.Obligations)
	}
}

// TestAPayloadThatWasOnlyReflectedIsRefuted pins the shape that matters
// most: the canary came back, and the response is the baseline with the
// payload printed into it.
func TestAPayloadThatWasOnlyReflectedIsRefuted(t *testing.T) {
	controls := []Control{
		MarkerGuardControl(),
		StatusControl(),
		BodyChangeControl(),
		ReflectionControl(),
	}

	decision := Decide(baselineExchange(), controls)
	if decision.Verdict != Refuted {
		t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, Refuted, decision.Obligations)
	}
}

// TestASatisfiedControlStillFacesTheAnchorRequirement pins the rule that two
// gates catching different mistakes is not redundancy for its own sake.
//
// The marker control is satisfied here and the body control is satisfied
// here, and the finding is still refuted: the response differs from the
// baseline by nothing but the echoed payload, and a control that was
// satisfied from the echo alone has not answered the question.
func TestASatisfiedControlStillFacesTheAnchorRequirement(t *testing.T) {
	controls := []Control{
		MarkerGuardControl(),
		BodyChangeControl(),
		ReflectionControl(),
	}

	decision := Decide(baselineExchange(), controls)
	if decision.Verdict != Refuted {
		t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, Refuted, decision.Obligations)
	}
	states := map[string]ObligationState{}
	for _, obligation := range decision.Obligations {
		states[obligation.Name] = obligation.State
	}
	if states["marker"] != Established {
		t.Fatalf("the marker obligation is %s, want %s", states["marker"], Established)
	}
	if states["body"] != Established {
		t.Fatalf("the body obligation is %s, want %s", states["body"], Established)
	}
	if states["reflection"] != Contradicted {
		t.Fatalf("the reflection obligation is %s, want %s", states["reflection"], Contradicted)
	}
}

// TestTheControlsArePure pins the promise that the same exchange and the
// same controls give the same decision every time.
func TestTheControlsArePure(t *testing.T) {
	exchange := executionExchange()
	controls := DefaultControls()

	first := Decide(exchange, controls)
	second := Decide(exchange, controls)

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two decisions over the same exchange differ:\n%v\n%v", first, second)
	}
}

// TestTheCallbackControlProvesABlindInteraction pins the only evidence a blind
// bug leaves: a request that arrived at the tester's address.
func TestTheCallbackControlProvesABlindInteraction(t *testing.T) {
	exchange := Observation{
		URL:          "https://target.example/fetch?u=http://abc.oast.pro/x",
		CallbackHost: "abc.oast.pro",
		Callbacks: []Callback{{
			Protocol:   "http",
			RemoteAddr: "198.51.100.7:44321",
			RawRequest: "GET /x HTTP/1.1\r\nHost: abc.oast.pro\r\nAuthorization: Bearer secret",
		}},
	}

	decision := Decide(exchange, []Control{CallbackControl()})
	if decision.Verdict != Proven {
		t.Fatalf("verdict is %s, want %s (obligations %v)", decision.Verdict, Proven, decision.Obligations)
	}
	evidence := decision.Obligations[0].Evidence
	if !strings.Contains(evidence, "http") || !strings.Contains(evidence, "198.51.100.7:44321") {
		t.Errorf("the evidence does not name the interaction: %q", evidence)
	}
	if !strings.Contains(evidence, "GET /x HTTP/1.1") {
		t.Errorf("the evidence carries no request line: %q", evidence)
	}
	if strings.Contains(evidence, "secret") {
		t.Errorf("the evidence quotes a credential: %q", evidence)
	}
}

// TestTheCallbackControlWithoutAnInteractionIsInconclusive pins the rule the
// whole path exists for: a quiet window is a check that did not answer, never
// a proof that the bug is absent.
func TestTheCallbackControlWithoutAnInteractionIsInconclusive(t *testing.T) {
	decision := Decide(Observation{CallbackHost: "abc.oast.pro"}, []Control{CallbackControl()})
	if decision.Verdict != Inconclusive {
		t.Fatalf("verdict is %s, want %s", decision.Verdict, Inconclusive)
	}
	if decision.Obligations[0].State != Missing {
		t.Errorf("the obligation is %s, want %s", decision.Obligations[0].State, Missing)
	}
	if !strings.Contains(decision.Obligations[0].Evidence, "abc.oast.pro") {
		t.Errorf("the obligation does not name the address it watched: %q", decision.Obligations[0].Evidence)
	}
}

// TestTheCallbackControlWithoutAnAddressIsMissing pins the difference between
// an address that stayed quiet and no address at all.
func TestTheCallbackControlWithoutAnAddressIsMissing(t *testing.T) {
	decision := Decide(Observation{Status: 200}, []Control{CallbackControl()})
	if decision.Verdict != Inconclusive {
		t.Fatalf("verdict is %s, want %s", decision.Verdict, Inconclusive)
	}
	if !strings.Contains(decision.Obligations[0].Evidence, "no callback address") {
		t.Errorf("the obligation does not say nothing was instrumented: %q", decision.Obligations[0].Evidence)
	}
}

// TestTheCallbackControlIsOptIn pins that the default set is unchanged: the
// question does not apply to a reflected finding, and a control that answers
// Missing by default would weaken every ordinary verdict.
func TestTheCallbackControlIsOptIn(t *testing.T) {
	for _, control := range DefaultControls() {
		if control.Name == "callback" {
			t.Fatal("the callback control is in the default set")
		}
	}
	found := false
	for _, control := range AllControls() {
		if control.Name == "callback" {
			found = true
		}
	}
	if !found {
		t.Fatal("the callback control is not nameable")
	}
}
