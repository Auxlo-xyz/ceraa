package llm

import (
	"strings"
	"testing"
)

// The bug this replaces: a 236000 default was applied to every model, so a
// 32k model and a 200k model were budgeted identically and the smaller one
// received a prompt it could not accept.
//
// The rule now: the configured limit governs and the catalog is a check that can
// only lower it. It corrects a limit too large for the model, and it protects a
// small model when nothing was configured. It never raises a limit, which is what
// let a session run to 137% of the window it was being displayed against while the
// compactor resolved the same figure against a 1M catalog entry and stayed silent.
func TestResolveContextWindowPrefersTheConfiguredLimit(t *testing.T) {
	cases := []struct {
		name       string
		configured int
		window     int
		want       int
		wantSource ContextWindowSource
	}{
		{"profile wins when stated", 200000, 400000, 200000, ContextFromProfile},
		// A profile below the advertised window is a deliberate local budget, not
		// a mistake for the catalog to correct upward.
		{"a conservative profile is not raised to the advertised window", 128000, 256000, 128000, ContextFromProfile},
		{"an advertised window equal to the limit is not a correction", 200000, 200000, 200000, ContextFromProfile},
		// The motivating mistake: a limit larger than the model can hold.
		{"catalog corrects a limit the model cannot hold", 512000, 256000, 256000, ContextFromCatalog},
		{"catalog protects an unset limit on a small model", 0, 32000, 32000, ContextFromCatalog},
		// An unset limit must not be inflated: the fallback stands.
		{"catalog never raises an unset limit", 0, 1000000, FallbackContextWindow, ContextFromDefault},
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

// A window must be reported as-is, including one small enough that a reserve
// would swallow it. Returning a reduced figure here double-counted headroom
// against the compactor's own buffer, which is what made a 32k model compact at
// 12% of its window.
func TestResolveContextWindowReturnsTheWindowWhole(t *testing.T) {
	for _, window := range []int{1, 512, 4096, 8192, 20001, 32000, 200000, 1000000} {
		got, source := ResolveContextWindow(window, ModelMetadata{ContextWindow: window})
		if got != window {
			t.Errorf("configured window %d resolved to %d, want it unchanged", window, got)
		}
		if source != ContextFromProfile {
			t.Errorf("configured window %d: source = %s, want %s", window, source, ContextFromProfile)
		}
	}
	// The same for a correction: the smaller advertised window is taken whole
	// rather than shaved, so headroom is still subtracted exactly once.
	for _, window := range []int{1, 512, 4096, 8192, 32000, 199999} {
		got, source := ResolveContextWindow(0, ModelMetadata{ContextWindow: window})
		if got != window {
			t.Errorf("advertised window %d resolved to %d, want it unchanged", window, got)
		}
		if source != ContextFromCatalog {
			t.Errorf("advertised window %d: source = %s, want %s", window, source, ContextFromCatalog)
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

// The floor is a product decision, not an implementation detail, so it is pinned
// by value rather than left to the constant it is. Every other test here compares
// against FallbackContextWindow, which means they all keep passing if the floor
// moves - so a silent change away from 200k would look
// like a clean run, and the conversation budget would shrink by a third with
// nothing reporting it.
//
// The floor assumes a 200k window for a model that may advertise less, which is
// the trade the constant's comment names. What must remain true whatever the
// value is: it is a real budget rather than zero, and the compactor's absolute
// reserve still fits inside it.
func TestTheContextWindowFloorIsTwoHundredThousand(t *testing.T) {
	if FallbackContextWindow != 200000 {
		t.Fatalf("FallbackContextWindow = %d, want 200000", FallbackContextWindow)
	}
	got, source := ResolveContextWindow(0, ModelMetadata{})
	if got != 200000 || source != ContextFromDefault {
		t.Errorf("ResolveContextWindow with nothing stated = %d/%s, want 200000/%s", got, source, ContextFromDefault)
	}
	// Still a window with room in it, not a figure that fills its own budget.
	if got <= 0 {
		t.Error("the floor resolved to zero, which is the failure this constant exists to prevent")
	}
}
