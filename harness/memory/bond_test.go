package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A missing bond file is a first run, not a failure.
func TestLiveBondMissingFileRendersNothing(t *testing.T) {
	render := LiveBond(filepath.Join(t.TempDir(), "BOND.md"), 2000)
	got, err := render()
	if err != nil {
		t.Fatalf("missing bond errored: %v", err)
	}
	if got != "" {
		t.Errorf("missing bond rendered %q, want nothing", got)
	}
}

// The bond renders wrapped with its law, so the read-only rule travels
// with the content.
func TestLiveBondWrapsContentWithItsLaw(t *testing.T) {
	path := filepath.Join(t.TempDir(), "BOND.md")
	if err := os.WriteFile(path, []byte("# BOND\n\nUser: Test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LiveBond(path, 2000)()
	if err != nil {
		t.Fatalf("bond errored: %v", err)
	}
	if !strings.HasPrefix(got, "<bond>\n") || !strings.Contains(got, "Sidekick-maintained") {
		t.Errorf("bond lost its wrapper or its law:\n%s", got)
	}
	if !strings.Contains(got, "User: Test") {
		t.Errorf("bond lost its content:\n%s", got)
	}
}

// Truncation takes from the body, never from the law.
func TestLiveBondTruncationKeepsTheLaw(t *testing.T) {
	path := filepath.Join(t.TempDir(), "BOND.md")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 5000)), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LiveBond(path, 2000)()
	if err != nil {
		t.Fatalf("bond errored: %v", err)
	}
	if len(got) > 2000 {
		t.Errorf("bond rendered %d bytes over a 2000 budget", len(got))
	}
	if !strings.Contains(got, "Sidekick-maintained") {
		t.Error("truncation dropped the read-only law")
	}
}

// Combine orders bond before memory and survives a failing source.
func TestCombineOrdersBondFirstAndSurvivesFailure(t *testing.T) {
	bond := func() (string, error) { return "BOND", nil }
	broken := func() (string, error) { return "", errors.New("disk gone") }
	mem := func() (string, error) { return "MEMORY", nil }
	got, err := Combine(bond, broken, mem, nil)()
	if err == nil {
		t.Error("the failure was swallowed entirely")
	}
	if strings.Index(got, "BOND") > strings.Index(got, "MEMORY") || !strings.Contains(got, "BOND") {
		t.Errorf("bond does not precede memory:\n%s", got)
	}
	if _, err := Combine(broken)(); err == nil {
		t.Error("all-failing combine reported no error")
	}
}
