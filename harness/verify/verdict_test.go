package verify

import "testing"

// decide must be total: every input reaches a verdict, and only PROVEN is
// reportable. That is the policy, so it is tested as the policy rather than by
// example.
func TestDecideIsTotalAndOnlyProvenIsReportable(t *testing.T) {
	cases := []struct {
		name       string
		hasBundle  bool
		rd         Rederivation
		want       Verdict
		reportable bool
	}{
		{
			name:      "no bundle suppresses",
			hasBundle: false,
			rd:        Rederivation{Attempted: true, Reachable: true, MarkerAvailable: true, MarkerInBaseline: true, MarkerPresent: true},
			want:      Unproven,
		},
		{
			name:      "never attempted holds rather than refuting",
			hasBundle: true,
			rd:        Rederivation{Reachable: true, MarkerAvailable: true, MarkerInBaseline: true, MarkerPresent: true},
			want:      Inconclusive,
		},
		{
			name:      "unreachable holds rather than refuting",
			hasBundle: true,
			rd:        Rederivation{Attempted: true, MarkerAvailable: true, MarkerInBaseline: true, MarkerPresent: true},
			want:      Inconclusive,
		},
		{
			name:      "nothing to check suppresses",
			hasBundle: true,
			rd:        Rederivation{Attempted: true, Reachable: true, MarkerInBaseline: true, MarkerPresent: true},
			want:      Unproven,
		},
		{
			name:      "marker only in the fresh response is not proof",
			hasBundle: true,
			rd:        Rederivation{Attempted: true, Reachable: true, MarkerAvailable: true, MarkerPresent: true},
			want:      Unproven,
		},
		{
			name:       "marker in both is proven",
			hasBundle:  true,
			rd:         Rederivation{Attempted: true, Reachable: true, MarkerAvailable: true, MarkerInBaseline: true, MarkerPresent: true},
			want:       Proven,
			reportable: true,
		},
		{
			name:      "marker gone on re-derivation is refuted",
			hasBundle: true,
			rd:        Rederivation{Attempted: true, Reachable: true, MarkerAvailable: true, MarkerInBaseline: true},
			want:      Refuted,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.hasBundle, tc.rd)
			if got.Verdict != tc.want {
				t.Fatalf("Decide = %s, want %s (%s)", got.Verdict, tc.want, got.Reason)
			}
			if got.Reportable() != tc.reportable {
				t.Fatalf("Reportable = %v, want %v for %s", got.Reportable(), tc.reportable, got.Verdict)
			}
			if tc.want != Proven && got.Reason == "" {
				t.Error("a suppressing verdict carried no reason; a reader cannot disagree with nothing")
			}
		})
	}
}

// The reason a verdict gives is the difference between "the check said no" and
// "the check never ran". Collapsing them is how a validator eats real findings,
// so the wording is part of the contract.
func TestTheReasonDistinguishesRefutedFromNeverChecked(t *testing.T) {
	refuted := Decide(true, Rederivation{Attempted: true, Reachable: true, MarkerAvailable: true, MarkerInBaseline: true})
	unreachable := Decide(true, Rederivation{Attempted: true, MarkerAvailable: true, MarkerInBaseline: true, MarkerPresent: true, Detail: "connection refused"})
	unproven := Decide(true, Rederivation{Attempted: true, Reachable: true, MarkerInBaseline: true, MarkerPresent: true})

	if refuted.Reason == unreachable.Reason || unreachable.Reason == unproven.Reason || refuted.Reason == unproven.Reason {
		t.Fatalf("three distinct outcomes shared a reason: %q / %q / %q", refuted.Reason, unreachable.Reason, unproven.Reason)
	}
	if unreachable.Reason != "connection refused" {
		t.Errorf("the specific detail was dropped in favour of %q", unreachable.Reason)
	}
}

// The false-Proven guard, stated on its own. A validator that finds its canary in
// a fresh response has proved the server echoes input, which is true of
// essentially every web server and is not a vulnerability.
func TestAMarkerPresentOnlyInTheFreshResponseCannotProveAnything(t *testing.T) {
	bundle := Bundle{
		Original: Exchange{
			Label:  "baseline",
			Status: 200,
			Body:   "<html><body>hello</body></html>",
		},
		Fresh: Exchange{
			Label:  "payload",
			Status: 200,
			Body:   "<html><body>CERAA-CANARY-42</body></html>",
		},
		FreshAttempted: true,
		FreshReachable: true,
	}
	got := bundle.Decide("CERAA-CANARY-42")
	if got.Verdict != Unproven {
		t.Fatalf("verdict = %s, want UNPROVEN: the marker was never in the original", got.Verdict)
	}
	if got.Reportable() {
		t.Fatal("a reflection was accepted as proof")
	}
}

// The same bundle with the marker in the original is the true positive. Both
// halves matter and the test pairs them so neither can drift alone.
func TestTheSameBundleProvesWhenTheMarkerWasInTheOriginalToo(t *testing.T) {
	bundle := Bundle{
		Original: Exchange{
			Label:  "original",
			Status: 200,
			Body:   "<html><body>CERAA-CANARY-42 executed</body></html>",
		},
		Fresh: Exchange{
			Label:  "re-derivation",
			Status: 200,
			Body:   "<html><body>CERAA-CANARY-42 executed</body></html>",
		},
		FreshAttempted: true,
		FreshReachable: true,
	}
	got := bundle.Decide("CERAA-CANARY-42")
	if got.Verdict != Proven || !got.Reportable() {
		t.Fatalf("verdict = %s (%s), want PROVEN", got.Verdict, got.Reason)
	}
}

// Execution is observed, not reflected. A payload sitting in the body is not
// evidence that anything ran.
func TestAnExecutionObservationCountsEvenWhenTheBodyDoesNotEchoIt(t *testing.T) {
	bundle := Bundle{
		Original: Exchange{
			Label:        "original",
			Status:       200,
			Body:         "<html><body>thanks</body></html>",
			Observations: []Observation{{Kind: "exec:dialog", Detail: "CERAA-CANARY-42"}},
		},
		Fresh: Exchange{
			Label:        "re-derivation",
			Status:       200,
			Body:         "<html><body>thanks</body></html>",
			Observations: []Observation{{Kind: "exec:dialog", Detail: "CERAA-CANARY-42"}},
		},
		FreshAttempted: true,
		FreshReachable: true,
	}
	if got := bundle.Decide("CERAA-CANARY-42"); got.Verdict != Proven {
		t.Fatalf("verdict = %s (%s): an observed dialog is the strongest evidence there is", got.Verdict, got.Reason)
	}
}
