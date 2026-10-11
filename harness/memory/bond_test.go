package memory

import (
	"errors"
	"strings"
	"testing"
)

// There is nothing left here to test about a bond: LiveBond, its law string and
// its <bond> block were deleted, and with them the three tests that pinned
// them. What remains is Combine, which chains whatever renderers it is handed
// and which has to survive one of them failing.

// Combine reports a failing source without blanking the others.
func TestCombineSurvivesAFailingSource(t *testing.T) {
	broken := func() (string, error) { return "", errors.New("disk gone") }
	mem := func() (string, error) { return "MEMORY", nil }
	got, err := Combine(broken, mem)()
	if err == nil {
		t.Error("the failure was swallowed entirely")
	}
	if !strings.Contains(got, "MEMORY") {
		t.Errorf("a failing source blanked the others: %q", got)
	}
	// Every source failing is still an error, not an empty string.
	if _, err := Combine(broken, broken)(); err == nil {
		t.Error("all-failing combine reported no error")
	}
}

// A nil renderer is a configured-off source, not a crash.
func TestCombineSkipsANilRenderer(t *testing.T) {
	got, err := Combine(nil, func() (string, error) { return "ONE", nil }, nil)()
	if err != nil {
		t.Fatalf("nil renderers errored: %v", err)
	}
	if got != "ONE\n" {
		t.Errorf("got %q, want the one live renderer", got)
	}
}
