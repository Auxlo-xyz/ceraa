package provision

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Proves provisioning works end to end, against the real release, rather than
// only compiling. Gated: it is a network test that fetches a few megabytes.
func TestLiveProvisionRipgrep(t *testing.T) {
	if os.Getenv("CERAA_LIVE_PROVISION") != "1" {
		t.Skip("set CERAA_LIVE_PROVISION=1; fetches the real release")
	}
	home := t.TempDir()
	t.Setenv("CERAA_HOME", home)
	// Make rg unreachable so the existing-PATH branch is skipped and the
	// download path is the one exercised.
	t.Setenv("PATH", "/nonexistent-for-this-test")
	start := time.Now()
	ok := EnsureRipgrep()
	t.Logf("EnsureRipgrep() = %v in %s (%s)", ok, time.Since(start).Round(time.Millisecond), ProvisionReason())
	if !ok {
		t.Fatal("provisioning failed: " + ProvisionReason())
	}
	bin := filepath.Join(home, "bin", "rg")
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("provisioned binary does not run: %v", err)
	}
	t.Logf("provisioned: %s", out)
	if _, err := os.Stat(bin); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Errorf("rg is not resolvable on PATH after provisioning: %v", err)
	}
}
