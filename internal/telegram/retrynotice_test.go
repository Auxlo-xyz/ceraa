package telegram

import (
	"errors"
	"strings"
	"testing"
)

// The notice goes inside a chat message, so an unbounded provider body would break
// the layout and bury the one fact the user needs.
func TestShortProviderErrorIsOneReadableLine(t *testing.T) {
	full := `provider error 502: {"error":{"message":"upstream failure","type":"bad_gateway_error","trace_id":"604a914d3bffd097c7c730a68a3cfd73"}}`
	got := shortProviderError(errors.New(full))
	if strings.Contains(got, "{") {
		t.Errorf("the raw provider body leaked into the notice: %q", got)
	}
	if len(got) > 83 {
		t.Errorf("the notice is %d chars, too long for a status line: %q", len(got), got)
	}
	if !strings.Contains(got, "502") {
		t.Errorf("the status code was lost, which is the useful part: %q", got)
	}
}

func TestShortProviderErrorHandlesNothingUseful(t *testing.T) {
	if got := shortProviderError(nil); got != "unknown" {
		t.Errorf("nil produced %q", got)
	}
	if got := shortProviderError(errors.New("   ")); got != "unknown" {
		t.Errorf("a blank error produced %q", got)
	}
	if got := shortProviderError(errors.New(strings.Repeat("x", 300))); len(got) > 83 {
		t.Errorf("a long error was not bounded: %d chars", len(got))
	}
}
