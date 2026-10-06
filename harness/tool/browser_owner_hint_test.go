package tool

import (
	"strings"
	"testing"
)

// These two calls are the ones a real session got wrong, and the model's reply
// afterwards was that the browser tool was unavailable. The message it received
// named what `open` accepts and stopped there, so it had nowhere to go. These
// pin that it is now handed the next call.
func TestAResjectedArgumentNamesTheActionThatTakesIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    map[string]any
		mustSay []string
		mustNot []string
	}{
		{
			name:    "submit on open",
			args:    map[string]any{"action": "open", "url": "https://example.com", "submit": true},
			mustSay: []string{`"submit" belongs to action=type with selector and text`, "its own call"},
		},
		{
			name:    "selector on open",
			args:    map[string]any{"action": "open", "url": "https://example.com", "selector": "#q"},
			mustSay: []string{`"selector" belongs to action=click or action=html`},
		},
		{
			name:    "text on open",
			args:    map[string]any{"action": "open", "url": "https://example.com", "text": "hi"},
			mustSay: []string{`"text" belongs to action=type with selector`},
		},
		{
			name:    "script on screenshot",
			args:    map[string]any{"action": "screenshot", "script": "1"},
			mustSay: []string{`"script" belongs to action=eval`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := new(BrowserTool).Translate(tc.args)
			if err == nil {
				t.Fatal("expected the call to be rejected")
			}
			msg := err.Error()
			for _, want := range tc.mustSay {
				if !strings.Contains(msg, want) {
					t.Errorf("message does not say %q:\n  %s", want, msg)
				}
			}
			for _, unwanted := range tc.mustNot {
				if strings.Contains(msg, unwanted) {
					t.Errorf("message should not contain %q:\n  %s", unwanted, msg)
				}
			}
			// The generic worked example used to be a fixed sequence that had
			// nothing to do with the mistake, which read as filler.
			if strings.Contains(msg, "for example action=open with url") {
				t.Errorf("message still carries the generic example:\n  %s", msg)
			}
		})
	}
}

// The owner index is derived, so it cannot drift from the argument spec. If
// someone adds an action without updating the hint, this fails.
func TestEveryArgumentHasAnOwnerThatAcceptsIt(t *testing.T) {
	for key, owners := range browserArgOwners {
		if len(owners) == 0 {
			t.Errorf("argument %q has no owning action", key)
		}
		for _, action := range owners {
			spec, ok := browserArgSpec[action]
			if !ok {
				t.Errorf("argument %q names action %q, which is not an action", key, action)
				continue
			}
			all := append(append([]string{}, spec.required...), spec.optional...)
			found := false
			for _, accepted := range all {
				if accepted == key {
					found = true
				}
			}
			if !found {
				t.Errorf("argument %q claims action %q owns it, but that action does not accept it", key, action)
			}
		}
	}
}

func TestAnArgumentNoActionTakesHasNoHint(t *testing.T) {
	if hint := browserArgOwnerHint("not_an_argument"); hint != "" {
		t.Errorf("hint = %q, want empty for an argument no action takes", hint)
	}
}

// The hint must not fire on a correct call.
func TestAValidCallIsUnaffected(t *testing.T) {
	if _, err := new(BrowserTool).Translate(map[string]any{"action": "type", "selector": "#q", "text": "hi", "submit": true}); err != nil {
		t.Fatalf("a valid call was rejected: %v", err)
	}
	if _, err := new(BrowserTool).Translate(map[string]any{"action": "open", "url": "https://example.com"}); err != nil {
		t.Fatalf("a valid call was rejected: %v", err)
	}
}
