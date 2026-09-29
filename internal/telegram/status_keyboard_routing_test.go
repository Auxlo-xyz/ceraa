package telegram

import (
	"encoding/json"
	"strings"
	"testing"

	"ceraa/internal/telegram/menus"
)

// "Sign in again" and "Switch to <provider>" were the same callback on all four
// subscription status keyboards, so both took the activate branch. A user who
// was already signed in and asked to re-authenticate was switched to the
// provider they were already on: no login, no prompt, no message.
//
// This is the same shape as the late-sign-in bug fixed in flowActivationStillWanted
// - a control that looks live and does something else - except here the routing
// was already correct and only the button was pointing at the wrong end of it.
// routeProviderCallback has had a distinct :relogin case since it was extracted,
// and menus.subscriptionMenu has always emitted it. Only these four keyboards
// were left on the shared callback.
func TestStatusKeyboardReloginIsNotTheActivateCallback(t *testing.T) {
	cases := []struct {
		name      string
		keyboard  map[string]any
		provider  string
		wantLabel string
	}{
		{"hermes", hermesStatusKeyboard(true), "hermes", "Sign in again"},
		{"xai", xaiStatusKeyboard(true), "xai", "Sign in again"},
		{"minimax", minimaxStatusKeyboard(true), "minimax", "Sign in again"},
		{"cline", clineStatusKeyboard(true), "cline", "Sign in again"},
	}

	for _, tc := range cases {
		buttons := buttonsIn(tc.keyboard)
		var relogin, switchData string
		for _, button := range buttons {
			switch {
			case strings.Contains(button["text"], tc.wantLabel):
				relogin = button["callback_data"]
			case strings.Contains(button["text"], "Switch to"):
				switchData = button["callback_data"]
			}
		}
		if relogin == "" {
			t.Errorf("%s: signed-in keyboard has no %q button: %v", tc.name, tc.wantLabel, buttons)
			continue
		}
		if switchData == "" {
			t.Errorf("%s: signed-in keyboard has no Switch button: %v", tc.name, buttons)
			continue
		}
		if relogin == switchData {
			t.Errorf("%s: %q and %q are both %q, so re-authenticating just switches",
				tc.name, "Switch to", tc.wantLabel, relogin)
		}

		// Assert against the router rather than the expected string, so this test
		// fails if the button and the route disagree rather than only if the
		// button stops being what someone typed here.
		providerID, kind, _ := routeProviderCallback(relogin)
		if providerID != tc.provider {
			t.Errorf("%s: relogin button routes to provider %q, want %q", tc.name, providerID, tc.provider)
		}
		if kind != providerRouteRelogin {
			t.Errorf("%s: relogin button resolves to kind %v, want a relogin route (activate=%v)",
				tc.name, kind, providerRouteActivate)
		}
		if _, kind, _ := routeProviderCallback(switchData); kind != providerRouteActivate {
			t.Errorf("%s: the Switch button resolves to kind %v, want activate", tc.name, kind)
		}
	}
}

// A signed-out keyboard must still offer the entry point that starts a login.
// Routing it at :relogin instead would be the mirror-image mistake: the button
// says sign in, and the activate branch does that when no credential exists.
func TestStatusKeyboardSignInWhenSignedOutReachesTheEntryPoint(t *testing.T) {
	cases := []struct {
		name     string
		keyboard map[string]any
		provider string
	}{
		{"hermes", hermesStatusKeyboard(false), "hermes"},
		{"xai", xaiStatusKeyboard(false), "xai"},
		{"minimax", minimaxStatusKeyboard(false), "minimax"},
		{"cline", clineStatusKeyboard(false), "cline"},
	}
	for _, tc := range cases {
		buttons := buttonsIn(tc.keyboard)
		if len(buttons) == 0 {
			t.Errorf("%s: signed-out keyboard has no buttons", tc.name)
			continue
		}
		for _, button := range buttons {
			data := button["callback_data"]
			if data == "" {
				continue
			}
			providerID, kind, _ := routeProviderCallback(data)
			if providerID != tc.provider {
				t.Errorf("%s: signed-out button %q routes to provider %q, want %q", tc.name, data, providerID, tc.provider)
			}
			// Activate, not relogin: the activate branch starts a login when there
			// is no credential, which is exactly what this state means.
			if kind != providerRouteActivate {
				t.Errorf("%s: signed-out button %q resolves to kind %v, want activate", tc.name, data, kind)
			}
		}
	}
}

// The hub's own provider screen already emitted :relogin, so the two renderers of
// the same control have now been made to agree. They are separate functions and
// they drifted once; this is the guard against it happening again quietly.
func TestHubAndStatusKeyboardsAgreeOnRelogin(t *testing.T) {
	hub := buttonsIn(menus.SubscriptionMenuForTest("Hermes", "hermes", true, false))
	var hubRelogin string
	for _, button := range hub {
		if strings.Contains(button["text"], "Sign in again") {
			hubRelogin = button["callback_data"]
		}
	}
	if hubRelogin == "" {
		t.Fatalf("the hub's provider screen has no re-login button: %v", hub)
	}
	for _, button := range buttonsIn(hermesStatusKeyboard(true)) {
		if strings.Contains(button["text"], "Sign in again") {
			if button["callback_data"] != hubRelogin {
				t.Errorf("the hub routes re-login to %q but the status keyboard uses %q",
					hubRelogin, button["callback_data"])
			}
		}
	}
}

// buttonsIn flattens an inline keyboard, tolerating a nil or malformed value so a
// test failure names the routing problem rather than a panic.
func buttonsIn(keyboard map[string]any) []map[string]string {
	rows, ok := keyboard["inline_keyboard"].([][]map[string]string)
	if !ok {
		return nil
	}
	var out []map[string]string
	for _, row := range rows {
		out = append(out, row...)
	}
	return out
}

// The keyboard must still be serialisable as markup. A builder that returns a
// shape Telegram cannot render fails silently at the send, which is how the
// permission prompt's oversized callback_data went unnoticed.
func TestStatusKeyboardsRenderAsMarkup(t *testing.T) {
	for name, keyboard := range map[string]map[string]any{
		"hermes-signed-in":  hermesStatusKeyboard(true),
		"hermes-signed-out": hermesStatusKeyboard(false),
		"xai-signed-in":     xaiStatusKeyboard(true),
		"minimax-signed-in": minimaxStatusKeyboard(true),
		"cline-signed-in":   clineStatusKeyboard(true),
	} {
		if _, err := json.Marshal(keyboard); err != nil {
			t.Errorf("%s does not serialise: %v", name, err)
		}
		if len(buttonsIn(keyboard)) == 0 {
			t.Errorf("%s rendered no buttons", name)
		}
	}
}
