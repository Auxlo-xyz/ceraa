package tool

import (
	"strings"
	"testing"
)

// A rejected browser call used to name only the offending argument. The model
// then retried the same shape until it concluded the browser tools were
// unavailable, and told the user they were missing. The message has to carry a
// recovery path.
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
			want: []string{"does not accept \"script\"", "accepts url, path, wait_ms", "one action"},
		},
		{
			name: "open rejects a screenshot argument",
			args: map[string]any{"action": "open", "url": "http://example.test", "full_page": true},
			want: []string{"does not accept \"full_page\"", "one action", "action=screenshot with full_page"},
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
