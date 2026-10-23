package tool

import (
	"strings"
	"testing"

	"ceraa/harness/netguard"
)

// The http/https-only rule is by design and stays; what has to change is the
// refusal itself: name the scheme that was detected, say the block is
// intentional, and point a caller who meant a local file at the CLI flag that opens one.
func TestValidateBrowserURLRefusalsExplainThemselves(t *testing.T) {
	t.Setenv(netguard.AllowPrivateEnv, "")
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{"file", "file:///etc/passwd", []string{"file:", "by design", "ceraa browser open --file"}},
		{"ftp", "ftp://x", []string{"ftp:", "by design", "ceraa browser open --file"}},
		{"javascript", "javascript:alert(1)", []string{"javascript:", "by design"}},
		{"bare host", "example.com", []string{`"example.com" does not look like a URL`, "https:// is required", "ceraa browser open --file"}},
		{"empty", "", []string{"url is required"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBrowserURL(tc.raw)
			if err == nil {
				t.Fatalf("validateBrowserURL(%q) was accepted", tc.raw)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

// A valid URL must get past the scheme check and on to netguard, not stop at it.
func TestValidateBrowserURLPassesAValidURLToTheGuard(t *testing.T) {
	t.Setenv(netguard.AllowPrivateEnv, "")
	if err := validateBrowserURL("https://example.com/docs"); err != nil {
		t.Fatalf("a public URL was refused: %v", err)
	}
}

// Both entry points funnel through validateBrowserURL, so one change covers
// both: prove it rather than assume it.
func TestBothBrowserPathsCarryTheSameRefusal(t *testing.T) {
	t.Setenv(netguard.AllowPrivateEnv, "")
	tool := NewBrowserTool()
	_, err := tool.Translate(map[string]any{"action": "open", "url": "file:///etc/passwd"})
	if err == nil || !strings.Contains(err.Error(), "ceraa browser open --file") {
		t.Errorf("Translate refusal = %v", err)
	}
	_, err = browserOpenTarget(map[string]any{"url": "javascript:alert(1)"})
	if err == nil || !strings.Contains(err.Error(), "javascript:") {
		t.Errorf("browserOpenTarget refusal = %v", err)
	}
}
