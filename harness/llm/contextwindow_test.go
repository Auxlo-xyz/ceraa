package llm

import (
	"strings"
	"testing"
)

// The bug this replaces: a 236000 default was applied to every model, so a
// 32k model and a 200k model were budgeted identically and the smaller one
// received a prompt it could not accept.
func TestResolveContextWindowUsesTheAdvertisedWindow(t *testing.T) {
	cases := []struct {
		name       string
		configured int
		window     int
		want       int
		wantSource ContextWindowSource
	}{
		{"profile wins when stated", 200000, 400000, 200000, ContextFromProfile},
		{"catalog supplies an unset profile", 0, 400000, 380000, ContextFromCatalog},
		// A flat 20k margin would leave 12k here, which is usable but throws away
		// a third of a small model's window for no reason.
		{"small model reserves a quarter, not the flat margin", 0, 32000, 24000, ContextFromCatalog},
		{"a 1M model is not clamped", 0, 1000000, 980000, ContextFromCatalog},
		{"unknown model gets the fallback", 0, 0, FallbackContextWindow, ContextFromDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, source := ResolveContextWindow(tc.configured, ModelMetadata{ContextWindow: tc.window})
			if got != tc.want || source != tc.wantSource {
				t.Errorf("ResolveContextWindow(%d, window=%d) = %d/%s, want %d/%s",
					tc.configured, tc.window, got, source, tc.want, tc.wantSource)
			}
		})
	}
}

// The reserve must never consume the window. A flat margin fails this at
// 20,001 tokens, where it leaves a single token of budget, and the resulting
// failure surfaces as a provider error on the first turn rather than as a
// budgeting mistake here.
func TestResolveContextWindowNeverStarvesTheBudget(t *testing.T) {
	for _, window := range []int{1, 512, 4096, 8192, 20000, 20001, 32000, 65536, 131072, 200000, 1000000} {
		got, source := ResolveContextWindow(0, ModelMetadata{ContextWindow: window})
		if source != ContextFromCatalog {
			t.Errorf("window %d: source = %s, want %s", window, source, ContextFromCatalog)
			continue
		}
		if got <= 0 {
			t.Errorf("window %d produced a budget of %d", window, got)
			continue
		}
		// At least three quarters of the window, by the reserve cap.
		if minimum := window - window/maxReserveDivisor; got < minimum {
			t.Errorf("window %d: budget %d is below the %d floor", window, got, minimum)
		}
		if got > window {
			t.Errorf("window %d: budget %d exceeds the window", window, got)
		}
	}
}

// The decision must be explainable from a log line, because a wrong budget with
// no stated provenance is indistinguishable from a provider bug.
func TestContextWindowDecisionStringCarriesProvenance(t *testing.T) {
	d := ContextWindowDecision{Limit: 180000, Source: ContextFromCatalog, Model: "some/model", AdvertisedWindow: 200000, Configured: 0}
	got := d.String()
	for _, want := range []string{"180000", "catalog", "some/model", "200000"} {
		if !strings.Contains(got, want) {
			t.Errorf("decision string %q is missing %q", got, want)
		}
	}
}
