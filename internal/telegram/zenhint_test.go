package telegram

import (
	"strings"
	"testing"
)

// A refusal has to name the fix, not just repeat the status.
//
// Zen says no three different ways and the generic message sent everyone to
// re-check a key that was never the problem. The default model is named because
// that is the one id measured to answer from outside the OpenCode app, so the
// hint has to lead to something that works rather than to another refusal.
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
