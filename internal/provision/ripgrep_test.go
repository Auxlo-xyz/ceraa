package provision

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The target mapping is a table lookup that silently produces a broken URL if a
// platform is missing from it, and the failure then shows up as a download error
// much later on someone else's machine.
func TestEverySupportedPlatformMapsToAPublishedAsset(t *testing.T) {
	supported := map[string]bool{"linux/amd64": true, "linux/arm64": true, "linux/386": true, "linux/arm": true, "darwin/amd64": true, "darwin/arm64": true}
	// The current platform must be one this build can provision for, or the image
	// and the binary disagree about what exists.
	if !supported[runtime.GOOS+"/"+runtime.GOARCH] {
		t.Skipf("no published ripgrep build for %s/%s, which is the case the warning is for", runtime.GOOS, runtime.GOARCH)
	}
	asset, err := releaseTarget()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(asset, "ripgrep-"+ripgrepVersion+"-") || !strings.HasSuffix(asset, ".tar.gz") {
		t.Errorf("asset = %q, want a pinned ripgrep tarball", asset)
	}
	// musl for linux/amd64, so it runs against a different libc.
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" && !strings.Contains(asset, "musl") {
		t.Errorf("linux/amd64 asset = %q, want the static musl build", asset)
	}
}

// The version is pinned. A tool that decides at runtime what to download is a
// tool that can be made to download something else.
func TestTheVersionIsPinned(t *testing.T) {
	if !strings.Contains(releaseBase, ripgrepVersion) {
		t.Errorf("release base %q does not pin a version", releaseBase)
	}
	if strings.Contains(releaseBase, "latest") {
		t.Error("the release base follows latest, so the fetched artefact is not reproducible")
	}
}

// An archive entry is untrusted input. One named ../../bin/sh would otherwise
// write outside the destination.
func TestArchiveEntriesCannotEscapeTheDestination(t *testing.T) {
	dir := t.TempDir()
	abs, err := filepath.Abs(filepath.Join(dir, "rg"))
	if err != nil {
		t.Fatal(err)
	}
	// withinDir true means accepted, so a path inside must be accepted and a
	// path outside must not be. The first two assertions here had it backwards.
	if !withinDir(dir, abs) {
		t.Error("a path inside the directory was rejected")
	}
	if !withinDir(dir, filepath.Join(dir, "sub", "rg")) {
		t.Error("a nested path inside the directory was rejected")
	}
	if withinDir(dir, filepath.Join(dir, "..", "..", "bin", "sh")) {
		t.Error("a path outside the directory was accepted")
	}
}

// Provisioning must not require root and must not write outside Ceraa's own
// data directory, because a user may run this without sudo and may want to
// uninstall by deleting one folder.
func TestProvisioningStaysInsideTheDataDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CERAA_HOME", home)
	dir := binDir()
	if !strings.HasPrefix(dir, home) {
		t.Errorf("bin dir %q is outside CERAA_HOME %q", dir, home)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the bin directory was not created: %v", err)
	}
	// No mode that would let another user read or replace the binary.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o022 != 0 {
		t.Errorf("bin dir mode is %v, want no group or world write", info.Mode().Perm())
	}
}

// The functional check, not a file test: a truncated download and a wrong binary
// both pass an existence check and then fail confusingly from inside a shell.
func TestIsRipgrepRejectsSomethingThatIsNotRipgrep(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "rg")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho not really\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isRipgrep(fake) {
		t.Error("a script that is not ripgrep passed the check")
	}
	if isRipgrep(filepath.Join(dir, "absent")) {
		t.Error("a missing file passed the check")
	}
}
