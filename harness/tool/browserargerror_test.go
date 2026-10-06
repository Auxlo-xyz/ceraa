package tool

import (
	"strings"
	"testing"
)

// A rejected browser call used to name only the offending argument. The model
// then retried the same shape until it concluded the browser tools were
// unavailable, and told the user they were missing. The message has to carry a
// recovery path, which means naming the action that does take the argument.
//
// The "open rejects a screenshot argument" case below used to be satisfied by a
// fixed worked example at the end of the message, which happened to contain the
// text "action=screenshot with full_page". It was green while the message named
// no owner at all, so it looked like coverage of a recovery path that did not
// exist. It asserts the real hint now, and the last case fails if any generic
// example creeps back in to satisfy these by accident.
func TestBrowserRejectionNamesWhatIsAccepted(t *testing.T) {
	tool := &BrowserTool{}
	cases := []struct {
		name string
		args map[string]any
		want []string
	}{
		{
			name: "open rejects an eval argument",
			args: map[string]any{"action": "open", "url": "http://example.test", "script": "1"},
			want: []string{"does not accept \"script\"", "accepts url, path, wait_ms", "one action",
				"\"script\" belongs to action=eval"},
		},
		{
			name: "open rejects a screenshot argument",
			args: map[string]any{"action": "open", "url": "http://example.test", "full_page": true},
			want: []string{"does not accept \"full_page\"", "one action",
				"\"full_page\" belongs to action=screenshot"},
		},
		{
			name: "click names its requirement",
			args: map[string]any{"action": "click", "text": "hi"},
			want: []string{"does not accept \"text\"", "requires selector"},
		},
		{
			name: "type names both requirements",
			args: map[string]any{"action": "type", "path": "/tmp/x.png"},
			want: []string{"requires selector and text"},
		},
		{
			name: "close takes nothing",
			args: map[string]any{"action": "close", "url": "http://example.test"},
			want: []string{"takes no arguments"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Translate(tc.args)
			if err == nil {
				t.Fatal("expected the call to be rejected")
			}
			message := err.Error()
			for _, want := range tc.want {
				if !strings.Contains(message, want) {
					t.Errorf("message %q does not contain %q", message, want)
				}
			}
		})
	}
}

// A fixed worked example is what made the case above pass for the wrong reason.
// It also does not help: the model had been shown a sequence and bundled
// everything into one call anyway. Nothing generic goes in the message.
func TestTheRejectionCarriesNoGenericWorkedExample(t *testing.T) {
	_, err := (&BrowserTool{}).Translate(
		map[string]any{"action": "open", "url": "http://example.test", "full_page": true})
	if err == nil {
		t.Fatal("expected the call to be rejected")
	}
	if strings.Contains(err.Error(), "for example") {
		t.Errorf("message still carries a generic example:\n  %s", err)
	}
}

func TestDescribeBrowserArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec struct {
			required []string
			optional []string
		}
		want string
	}{
		{"none", struct {
			required []string
			optional []string
		}{}, "it takes no arguments"},
		{"optional only", struct {
			required []string
			optional []string
		}{optional: []string{"a", "b"}}, "it accepts a, b"},
		{"required only", struct {
			required []string
			optional []string
		}{required: []string{"a", "b"}}, "it requires a and b"},
		{"both", struct {
			required []string
			optional []string
		}{required: []string{"a"}, optional: []string{"b"}}, "it requires a, and also accepts b"},
	} {
		if got := describeBrowserArgs(tc.spec); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
