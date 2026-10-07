package verify

import (
	"regexp"
	"strings"
	"testing"
)

// crossIdentity is the control worth getting right, because the way it is faked
// is the single most common IDOR false positive: the tester reads their own data
// and calls it a breach.
func TestCrossIdentityNeedsTwoIdentitiesAndADeniedAnonymousProbe(t *testing.T) {
	bundle := Bundle{
		Original: Exchange{
			Label:  "attacker",
			Status: 200,
			Body:   `{"email":"victim@corp.example","invoice":4711}`,
		},
		Fresh: Exchange{
			Label:  "anon",
			Status: 404,
			Body:   "not found",
		},
		FreshAttempted: true,
		FreshReachable: true,
	}
	req := Request{
		Control:          ControlCrossIdentity,
		Marker:           "victim@corp.example",
		AttackerLabel:    "attacker",
		AnonymousLabel:   "anon",
		AttackerIdentity: "session A (low privilege)",
		VictimMarker:     "victim@corp.example",
	}
	got := Evaluate(bundle, req)
	if !got.Satisfied {
		t.Fatalf("control failed: %s", got.Missing)
	}
	if decision, _ := VerifyDetailed(bundle, req); !decision.Reportable() {
		t.Fatalf("verdict = %s (%s), want reportable", decision.Verdict, decision.Reason)
	}
}

// The object being public is a different finding at a different severity, and
// the control exists to say so rather than to pass it.
func TestCrossIdentityRefusesWhenTheObjectIsPublicToEveryone(t *testing.T) {
	bundle := Bundle{
		Original:       Exchange{Label: "attacker", Status: 200, Body: "victim@corp.example"},
		Fresh:          Exchange{Label: "anon", Status: 200, Body: "victim@corp.example"},
		FreshAttempted: true,
		FreshReachable: true,
	}
	got := Evaluate(bundle, Request{
		Control:          ControlCrossIdentity,
		Marker:           "victim@corp.example",
		AttackerLabel:    "attacker",
		AnonymousLabel:   "anon",
		AttackerIdentity: "session A",
		VictimMarker:     "victim@corp.example",
	})
	if got.Satisfied {
		t.Fatal("a public object was accepted as an access-control break")
	}
	if got.Kind != Contradicted {
		t.Errorf("kind = %s, want CONTRADICTED: the evidence was collected and it says no", got.Kind)
	}
	if !strings.Contains(got.Summary, "public") {
		t.Errorf("the reason does not say the object is public: %q", got.Summary)
	}
}

// Missing the anonymous control is held, not passed. A control that was never run
// has not cleared anything.
func TestCrossIdentityHoldsWhenTheAnonymousControlWasNeverRun(t *testing.T) {
	bundle := Bundle{
		Original:       Exchange{Label: "attacker", Status: 200, Body: "victim@corp.example"},
		FreshAttempted: true,
		FreshReachable: true,
	}
	req := Request{
		Control:          ControlCrossIdentity,
		Marker:           "victim@corp.example",
		AttackerLabel:    "attacker",
		AnonymousLabel:   "anon",
		AttackerIdentity: "session A",
		VictimMarker:     "victim@corp.example",
	}
	got := Evaluate(bundle, req)
	if got.Satisfied {
		t.Fatal("the control passed with no anonymous exchange recorded")
	}
	decision := Verify(bundle, req)
	if decision.Verdict != Inconclusive {
		t.Fatalf("verdict = %s, want INCONCLUSIVE: an unrun control has not refuted anything", decision.Verdict)
	}
}

// Reflection is not execution. The body carrying the payload is the signature of
// an unverified XSS claim and the control has to name it as such.
func TestExecutionRefusesReflectionAndSaysSo(t *testing.T) {
	// Observations of other kinds were recorded, so the channel was watched and
	// did not fire. That is a contradiction. An exchange with no observations at
	// all would be Missing instead, and the two are told apart on purpose.
	bundle := Bundle{
		Original: Exchange{
			Label: "original", Status: 200,
			Body:         "<input value=\"CERAA-CANARY-42\">",
			Observations: []Observation{{Kind: "console", Detail: "page loaded"}},
		},
		Fresh: Exchange{
			Label: "fresh", Status: 200,
			Body:         "<input value=\"CERAA-CANARY-42\">",
			Observations: []Observation{{Kind: "console", Detail: "page loaded"}},
		},
		FreshAttempted: true,
		FreshReachable: true,
	}
	got := Evaluate(bundle, Request{
		Control:          ControlExecution,
		Marker:           "CERAA-CANARY-42",
		ExecutionChannel: "exec:dialog",
	})
	if got.Satisfied {
		t.Fatal("a reflected payload was accepted as execution")
	}
	if got.Kind != Contradicted {
		t.Errorf("kind = %s, want CONTRADICTED: the channel was watched and did not fire", got.Kind)
	}
	joined := strings.Join(unmetText(got), " ")
	if !strings.Contains(joined, "reflection") {
		t.Errorf("the reason does not identify reflection as the problem: %q", joined)
	}
}

// An unwatched channel has not answered anything. Reporting it as a contradiction
// would tell the reader the payload was tested and failed, which is a different
// and much stronger claim than "nobody looked".
func TestAnUnwatchedExecutionChannelIsMissingNotContradicted(t *testing.T) {
	bundle := Bundle{
		Original:       Exchange{Label: "original", Status: 200, Body: "<html></html>"},
		FreshAttempted: true,
		FreshReachable: true,
	}
	got := Evaluate(bundle, Request{Control: ControlExecution, Marker: "CERAA-CANARY-42"})
	if got.Kind != Missing {
		t.Fatalf("kind = %s, want MISSING: no observations were recorded at all", got.Kind)
	}
	if Verify(bundle, Request{Control: ControlExecution, Marker: "CERAA-CANARY-42"}).Verdict != Inconclusive {
		t.Fatal("an untested finding was treated as settled either way")
	}
}

func TestExecutionPassesOnAnObservedChannel(t *testing.T) {
	bundle := Bundle{
		Original:       Exchange{Label: "original", Status: 200, Observations: []Observation{{Kind: "exec:dialog", Detail: "CERAA-CANARY-42"}}},
		Fresh:          Exchange{Label: "fresh", Status: 200, Observations: []Observation{{Kind: "exec:dialog", Detail: "CERAA-CANARY-42"}}},
		FreshAttempted: true,
		FreshReachable: true,
	}
	got := Evaluate(bundle, Request{Control: ControlExecution, Marker: "CERAA-CANARY-42", ExecutionChannel: "exec:dialog"})
	if !got.Satisfied {
		t.Fatalf("control failed on a real execution: %s", got.Missing)
	}
}

// A signature that appears on the benign baseline too is a banner. One
// direction of the differential is not enough.
func TestDifferentialRefusesASignalThatIsPresentOnTheBaselineToo(t *testing.T) {
	bundle := Bundle{
		Original:       Exchange{Label: "payload", Status: 500, Body: "SQLSTATE[42000]: syntax error"},
		Fresh:          Exchange{Label: "baseline", Status: 500, Body: "SQLSTATE[42000]: syntax error"},
		FreshAttempted: true,
		FreshReachable: true,
	}
	got := Evaluate(bundle, Request{
		Control:       ControlDifferential,
		Marker:        "SQLSTATE",
		PayloadLabel:  "payload",
		BaselineLabel: "baseline",
	})
	if got.Satisfied {
		t.Fatal("a signal present on the benign baseline was accepted as caused by the payload")
	}
	if got.Kind != Contradicted {
		t.Errorf("kind = %s, want CONTRADICTED", got.Kind)
	}
	if !strings.Contains(got.Summary, "not caused by the payload") {
		t.Errorf("the reason does not explain why the signal proves nothing: %q", got.Summary)
	}
}

func TestDifferentialPassesWhenTheSignalIsOnlyOnThePayload(t *testing.T) {
	bundle := Bundle{
		Original:       Exchange{Label: "payload", Status: 500, Body: "SQLSTATE[42000]: syntax error"},
		Fresh:          Exchange{Label: "baseline", Status: 200, Body: "ok"},
		FreshAttempted: true,
		FreshReachable: true,
	}
	got := Evaluate(bundle, Request{
		Control:       ControlDifferential,
		Marker:        "SQLSTATE",
		Signal:        "SQLSTATE",
		PayloadLabel:  "payload",
		BaselineLabel: "baseline",
	})
	if !got.Satisfied {
		t.Fatalf("control failed on a clean differential: %s", got.Missing)
	}
}

// A reflected parameter that never becomes a Location is the most common
// open-redirect false positive there is.
func TestRedirectRefusesA200ThatMerelyEchoesTheParameter(t *testing.T) {
	bundle := Bundle{
		Original:       Exchange{Label: "payload", Status: 200, Body: "continue to https://evil.example/"},
		Fresh:          Exchange{Label: "fresh", Status: 200, Body: "continue to https://evil.example/"},
		FreshAttempted: true,
		FreshReachable: true,
	}
	got := Evaluate(bundle, Request{Control: ControlRedirect, ExpectedHost: "evil.example"})
	if got.Satisfied {
		t.Fatal("a 200 that echoed the parameter was accepted as an open redirect")
	}
}

func TestRedirectPassesWhenLocationResolvesToTheAttackerHost(t *testing.T) {
	bundle := Bundle{
		Original:       Exchange{Label: "payload", URL: "https://target.example/login", Status: 302, ResponseHeaders: map[string]string{"Location": "https://evil.example/next"}},
		Fresh:          Exchange{Label: "fresh", URL: "https://target.example/login", Status: 302, ResponseHeaders: map[string]string{"Location": "https://evil.example/next"}},
		FreshAttempted: true,
		FreshReachable: true,
	}
	got := Evaluate(bundle, Request{Control: ControlRedirect, ExpectedHost: "evil.example"})
	if !got.Satisfied {
		t.Fatalf("control failed on a real redirect: %s", got.Missing)
	}
}

// Header names are case-insensitive on the wire. An exact lookup would turn a
// real finding into "no Location header", which is a false negative dressed as
// a check.
func TestHeaderLookupIsCaseInsensitive(t *testing.T) {
	exchange := Exchange{ResponseHeaders: map[string]string{"location": "https://evil.example/"}}
	if got := exchange.HeaderValue("Location"); got != "https://evil.example/" {
		t.Fatalf("HeaderValue = %q, want the value regardless of case", got)
	}
}

// A reachable path is not a disclosure.
func TestContentRefusesA200ThatServesAnApplicationShell(t *testing.T) {
	bundle := Bundle{
		Original:       Exchange{Label: "original", Status: 200, Body: "<!doctype html><html><body><div id=root></div></body></html>"},
		Fresh:          Exchange{Label: "fresh", Status: 200, Body: "<!doctype html><html><body><div id=root></div></body></html>"},
		FreshAttempted: true,
		FreshReachable: true,
	}
	got := Evaluate(bundle, Request{Control: ControlContent})
	if got.Satisfied {
		t.Fatal("an application shell was accepted as a secret disclosure")
	}
}

func TestContentPassesOnARecognisedCredentialAndMasksIt(t *testing.T) {
	bundle := Bundle{
		Original:       Exchange{Label: "original", Status: 200, Body: `AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE`},
		Fresh:          Exchange{Label: "fresh", Status: 200, Body: `AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE`},
		FreshAttempted: true,
		FreshReachable: true,
	}
	got := Evaluate(bundle, Request{Control: ControlContent})
	if !got.Satisfied {
		t.Fatalf("control failed on a real key: %s", got.Missing)
	}
	joined := strings.Join(obligationText(got), " ")
	if strings.Contains(joined, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("the finding quotes the credential in full: %q", joined)
	}
	if !strings.Contains(joined, "AKIAIO") {
		t.Errorf("the match was not reported at all: %q", joined)
	}
}

// Nothing in-band can confirm a blind bug. No callback is never a confirmation.
func TestCallbackRefusesAnInjectedPayloadWithNoInteraction(t *testing.T) {
	bundle := Bundle{
		Original:       Exchange{Label: "original", Status: 200, Body: "queued"},
		Fresh:          Exchange{Label: "fresh", Status: 200, Body: "queued"},
		FreshAttempted: true,
		FreshReachable: true,
	}
	got := Evaluate(bundle, Request{Control: ControlCallback, Marker: "CERAA-CANARY-42"})
	if got.Satisfied {
		t.Fatal("a payload with no callback was accepted as a blind vulnerability")
	}
}

func TestCallbackPassesOnACorrelatedInteraction(t *testing.T) {
	observation := []Observation{{Kind: "oob:dns", Detail: "CERAA-CANARY-42.ceraa.invalid"}}
	bundle := Bundle{
		Original:       Exchange{Label: "original", Status: 200, Observations: observation},
		Fresh:          Exchange{Label: "fresh", Status: 200, Observations: observation},
		FreshAttempted: true,
		FreshReachable: true,
	}
	got := Evaluate(bundle, Request{Control: ControlCallback, Marker: "CERAA-CANARY-42"})
	if !got.Satisfied {
		t.Fatalf("control failed on a real callback: %s", got.Missing)
	}
}

// Two gates catching different mistakes. A control can be satisfied entirely
// from the re-derivation, and a proof with no anchor in the original observation
// is the reflection-versus-execution confusion wearing a different hat. The
// marker-in-original rule applies on top of the control, not instead of it.
func TestAControlSatisfiedOnlyByTheReDerivationIsNotProven(t *testing.T) {
	bundle := Bundle{
		// The original exchange is the anonymous control: denied, and carrying no
		// marker. The only exchange that ever held the victim's marker is the
		// re-derivation.
		Original:       Exchange{Label: "anon", Status: 403, Body: "forbidden"},
		Fresh:          Exchange{Label: "attacker", Status: 200, Body: "victim@corp.example"},
		FreshAttempted: true,
		FreshReachable: true,
	}
	req := Request{
		Control:          ControlCrossIdentity,
		Marker:           "victim@corp.example",
		AttackerLabel:    "attacker",
		AnonymousLabel:   "anon",
		AttackerIdentity: "session A",
		VictimMarker:     "victim@corp.example",
	}
	decision, control := VerifyDetailed(bundle, req)
	if control.Kind != Established {
		t.Fatalf("the control should be satisfied by this bundle: %s / %s", control.Kind, control.Missing)
	}
	if decision.Reportable() {
		t.Fatalf("a passed control bypassed the anchor requirement: %s", decision.Reason)
	}
	if decision.Verdict != Unproven {
		t.Fatalf("verdict = %s, want UNPROVEN", decision.Verdict)
	}
}

// The default patterns have to stay in step with harness/redact. A second,
// disagreeing list of what a credential looks like is a bug waiting to happen.
func TestDefaultSecretPatternsCompile(t *testing.T) {
	for _, pattern := range DefaultSecretPatterns() {
		if _, err := regexp.Compile(pattern); err != nil {
			t.Errorf("pattern %q does not compile: %v", pattern, err)
		}
	}
}

// unmetText returns the details of obligations that did not resolve as
// established.
func unmetText(result ControlResult) []string {
	out := make([]string, 0, len(result.Obligations))
	for _, obligation := range result.Obligations {
		if !obligation.Met() {
			out = append(out, obligation.Detail)
		}
	}
	return out
}

// obligationText returns every obligation's detail, met or not.
func obligationText(result ControlResult) []string {
	out := make([]string, 0, len(result.Obligations))
	for _, obligation := range result.Obligations {
		if obligation.Detail != "" {
			out = append(out, obligation.Detail)
		}
	}
	return out
}
