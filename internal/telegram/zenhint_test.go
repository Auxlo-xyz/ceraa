package telegram

import (
	"strings"
	"testing"
)

// A refusal has to name the fix, not just repeat the status.
//
// Zen says no several ways and the generic message sent everyone to re-check
// a key that was never the problem. The default model is named because that is
// an id measured to answer from outside the OpenCode app with the full gateway
// contract in place, so the hint leads to something that works rather than to
// another refusal.
func TestZenRefusalsSayWhatToDo(t *testing.T) {
	cases := map[string]struct {
		body   string
		wanted []string
	}{
		"free tier": {
			body:   `{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`,
			wanted: []string{"inside the OpenCode app", "space-bunny-free"},
		},
		"no funds": {
			body:   `{"error":{"message":"Upstream request failed: Insufficient account funds"}}`,
			wanted: []string{"no balance", "costs nothing"},
		},
		"go subscription": {
			body:   `{"error":{"message":"Upstream request failed: An active OpenCode Go subscription is required to use Go models."}}`,
			wanted: []string{"subscription", "do not"},
		},
		"bad key": {
			body:   `{"error":{"type":"AuthError","message":"Invalid API key."}}`,
			wanted: []string{"did not accept that key"},
		},
		"deprecated": {
			body:   `{"type":"error","error":{"type":"ModelDeprecated","message":"Model exo-free has been deprecated."}}`,
			wanted: []string{"retired", "from Model"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			hint := providerFailureHint([]byte(tc.body))
			if hint == "" {
				t.Fatalf("no hint for %s", name)
			}
			for _, want := range tc.wanted {
				if !strings.Contains(hint, want) {
					t.Errorf("hint missing %q: %s", want, hint)
				}
			}
		})
	}
}

// An error the code does not know must not get a made-up instruction.
func TestAnUnknownRefusalGetsNoInventedFix(t *testing.T) {
	if hint := providerFailureHint([]byte(`{"error":{"message":"teapot"}}`)); hint != "" {
		t.Errorf("invented a fix for an unrecognised error: %s", hint)
	}
}

// Two more refusals Zen issues, both measured live on 2026-10-01 once the header
// and body contract were satisfied: a model that does not speak this protocol
// answers 400 ModelProtocolUnsupported, and one with no path on this endpoint
// answers 404 with a route echo. Neither is a key problem, and an unknown-error
// empty hint would have told the user nothing at all.
func TestZenProtocolAndRouteRefusalsSayWhatToDo(t *testing.T) {
	cases := map[string]struct {
		body   string
		wanted []string
	}{
		"protocol unsupported": {
			body:   `{"type":"error","error":{"type":"ModelProtocolUnsupported","message":"Model does not support this protocol."}}`,
			wanted: []string{"chat/completions", "space-bunny-free"},
		},
		"no route": {
			body:   `{"status":404,"message":"Cannot find any route matching [POST] https://opencode.ai/zen/v1/chat/completions"}`,
			wanted: []string{"chat/completions", "space-bunny-free"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			hint := providerFailureHint([]byte(tc.body))
			if hint == "" {
				t.Fatalf("no hint for %s", name)
			}
			for _, want := range tc.wanted {
				if !strings.Contains(hint, want) {
					t.Errorf("hint missing %q: %s", want, hint)
				}
			}
		})
	}
}

// A 404 from some other endpoint must not be read as "this model has no route
// here". The real message names chat/completions because that is the path Zen
// could not match, and requiring it keeps a generic not-found out of the hint.
func TestARoute404OnAnotherEndpointGetsNoHint(t *testing.T) {
	body := `{"status":404,"message":"Cannot find any route matching [GET] https://opencode.ai/zen/v1/models"}`
	if hint := providerFailureHint([]byte(body)); hint != "" {
		t.Errorf("invented a model hint for a different endpoint: %s", hint)
	}
}
