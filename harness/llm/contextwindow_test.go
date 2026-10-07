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
		// The window is returned whole. Headroom is an absolute requirement and is
		// subtracted by whoever needs it, not proportionally shaved off here.
		{"catalog supplies an unset profile", 0, 400000, 400000, ContextFromCatalog},
		{"small window is not padded", 0, 32000, 32000, ContextFromCatalog},
		{"a 1M model is not clamped", 0, 1000000, 1000000, ContextFromCatalog},
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
func TestResolveContextWindowNeverReducesTheWindow(t *testing.T) {
	for _, window := range []int{1, 512, 4096, 8192, 20001, 32000, 200000, 1000000} {
		got, source := ResolveContextWindow(0, ModelMetadata{ContextWindow: window})
		if got != window {
			t.Errorf("window %d resolved to %d, want it unchanged", window, got)
		}
		if source != ContextFromCatalog {
			t.Errorf("window %d: source = %s, want %s", window, source, ContextFromCatalog)
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
// moves - so a silent change from 200k back to a conservative 128k would look
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
