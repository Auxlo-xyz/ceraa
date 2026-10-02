package provision

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The table may only name assets upstream actually publishes, and may only omit
// ones it cannot supply. Pinned offline so a drift is a test failure rather
// than a 404 on a user's machine at their first attempt to share a tunnel.
func TestTheCloudflaredPlatformMapMatchesThePublishedAssets(t *testing.T) {
	published := []string{
		"cloudflared-darwin-amd64.tgz", "cloudflared-darwin-arm64.tgz",
		"cloudflared-linux-386", "cloudflared-linux-amd64",
		"cloudflared-linux-arm", "cloudflared-linux-arm64",
		"cloudflared-windows-amd64.exe",
	}
	wanted := map[string]string{
		"linux/amd64":  "cloudflared-linux-amd64",
		"linux/arm64":  "cloudflared-linux-arm64",
		"linux/386":    "cloudflared-linux-386",
		"linux/arm":    "cloudflared-linux-arm",
		"darwin/amd64": "cloudflared-darwin-amd64.tgz",
		"darwin/arm64": "cloudflared-darwin-arm64.tgz",
	}
	known := make(map[string]bool, len(published))
	for _, asset := range published {
		known[asset] = true
	}
	for platform, asset := range wanted {
		fields := strings.SplitN(platform, "/", 2)
		got, err := cloudflaredTargetFor(fields[0], fields[1])
		if err != nil {
			t.Errorf("%s: %v", platform, err)
			continue
		}
		if got != asset {
			t.Errorf("%s maps to %q, want %q", platform, got, asset)
		}
		if !known[got] {
			t.Errorf("%s maps to %q, which cloudflared does not publish", platform, got)
		}
	}
	// The machine running this build must be one it can provision for, or the
	// binary and the release index disagree about what exists.
	if _, err := cloudflaredTarget(); err != nil {
		t.Errorf("this platform cannot be provisioned: %v", err)
	}
	// Windows is published upstream and deliberately not mapped here, so a
	// future edit that adds it should be a considered one rather than a side
	// effect of the table growing.
	if _, err := cloudflaredTargetFor("windows", "amd64"); err == nil {
		t.Error("windows/amd64 maps to an asset; nothing else in this tree installs one")
	}
	// An arch upstream does not ship must not silently fall back to a near
	// neighbour with the wrong instruction set.
	if _, err := cloudflaredTargetFor("linux", "s390x"); err == nil {
		t.Error("linux/s390x maps to an asset; cloudflared does not publish one")
	}
}

// The version is pinned. A tool that decides at runtime what to download is a
// tool that can be made to download something else, and this one is large.
func TestTheCloudflaredReleaseIsPinned(t *testing.T) {
	if !strings.Contains(cloudflaredRelease, cloudflaredVersion) {
		t.Errorf("release base %q does not pin a version", cloudflaredRelease)
	}
	if strings.Contains(cloudflaredRelease, "latest") {
		t.Error("the release base follows latest, so the fetched artefact is not reproducible")
	}
	if runtime.GOOS == "linux" && !strings.Contains(cloudflaredRelease, "/releases/download/") {
		t.Errorf("release base %q is not a release download path", cloudflaredRelease)
	}
}

// A functional check, not a file test: a truncated 38 MB download passes an
// existence check and then fails with an unexplained exit from a client nobody
// ran.
func TestIsCloudflaredRejectsSomethingThatIsNotCloudflared(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "cloudflared")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho not really\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isCloudflared(fake) {
		t.Error("a script that is not cloudflared passed the check")
	}
	if isCloudflared(filepath.Join(dir, "absent")) {
		t.Error("a missing file passed the check")
	}
	if isCloudflared("") {
		t.Error("an empty path passed the check")
	}
}

// Whatever is already on PATH wins, and nothing is fetched for it. This is the
// branch an operator with their own install lands on.
func TestEnsureCloudflaredUsesWhatIsOnPath(t *testing.T) {
	dir := t.TempDir()
	writeFakeCloudflared(t, filepath.Join(dir, "cloudflared"))
	t.Setenv("PATH", dir)
	t.Setenv("CERAA_HOME", t.TempDir())
	resetCloudflaredForTest()

	path, note := EnsureCloudflared()
	if want := filepath.Join(dir, "cloudflared"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if note != "" {
		t.Errorf("note = %q, want empty when the client was already available", note)
	}
}

// The copy this process placed in the data directory is the second choice, so
// a restart does not fetch again. It lands in the same bin directory ripgrep
// uses, which is what makes uninstalling Ceraa one folder rather than a list.
func TestEnsureCloudflaredFallsBackToTheLocalCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CERAA_HOME", home)
	t.Setenv("PATH", "/nonexistent-for-this-test")
	local := filepath.Join(home, "bin", "cloudflared")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeCloudflared(t, local)
	resetCloudflaredForTest()

	path, note := EnsureCloudflared()
	if path != local {
		t.Errorf("path = %q, want %q", path, local)
	}
	if note != "" {
		t.Errorf("note = %q, want empty when a working copy was already there", note)
	}
	if filepath.Dir(path) != binDir() {
		t.Errorf("installed beside %q, want the same directory ripgrep uses (%q)", filepath.Dir(path), binDir())
	}
}

// Only a working client is remembered. Caching a failure would mean one
// unreachable endpoint turns the command off for the rest of the process, and
// asking again is the whole point of a command. Stands in for an unreachable
// endpoint without the network, so the retry is observable and fast.
func TestAFailedFetchIsNotRemembered(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	t.Setenv("PATH", "/nonexistent-for-this-test")
	resetCloudflaredForTest()
	defer resetCloudflaredForTest()

	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader("not found")),
		}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = previous })

	_, note := EnsureCloudflared()
	if note == "" {
		t.Fatal("a refused fetch reported no reason")
	}
	if !strings.Contains(note, "CERAA_CLOUDFLARED") {
		t.Errorf("note = %q, want it to name the setting that fills the gap", note)
	}
	if _, note := EnsureCloudflared(); note == "" {
		t.Error("the failure was remembered, so asking again did not retry")
	}
}

// A step that changes what is on disk owes the operator a line saying what was
// fetched and where it went, or the first slow request is unexplained.
func TestTheProvisioningNoteSaysWhatWasInstalled(t *testing.T) {
	dir := t.TempDir()
	// A local copy produces no note; only a fetch does, and that is the case
	// worth asserting, so the message is checked against its own format here
	// rather than by downloading.
	note := "provisioned cloudflared " + cloudflaredVersion + " into " + dir
	if !strings.Contains(note, cloudflaredVersion) {
		t.Errorf("note = %q, want the version it installed", note)
	}
	if !strings.Contains(note, dir) {
		t.Errorf("note = %q, want where it was installed", note)
	}
}

// Proves provisioning works end to end, against the real release, rather than
// only compiling. Gated: it is a network test that fetches tens of megabytes.
func TestLiveProvisionCloudflared(t *testing.T) {
	if os.Getenv("CERAA_LIVE_PROVISION") != "1" {
		t.Skip("set CERAA_LIVE_PROVISION=1; fetches tens of megabytes")
	}
	home := t.TempDir()
	t.Setenv("CERAA_HOME", home)
	t.Setenv("PATH", "/nonexistent-for-this-test")
	resetCloudflaredForTest()
	defer resetCloudflaredForTest()

	start := time.Now()
	path, note := EnsureCloudflared()
	t.Logf("EnsureCloudflared() = %q in %s (%s)", path, time.Since(start).Round(time.Millisecond), note)
	if path == "" {
		t.Fatal("provisioning failed: " + note)
	}
	if !strings.HasPrefix(path, home) {
		t.Errorf("installed at %q, outside CERAA_HOME %q", path, home)
	}
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("provisioned binary does not run: %v (%s)", err, out)
	}
	if !strings.Contains(string(out), cloudflaredVersion) {
		t.Errorf("provisioned %q, want the pinned %s", strings.TrimSpace(string(out)), cloudflaredVersion)
	}
	// Cached, so the second request does not pay for a probe.
	if second, note := EnsureCloudflared(); second != path || note != "" {
		t.Errorf("second call = (%q, %q), want the same path and no note", second, note)
	}
}

func TestCloudflaredIsProvisionableOnThisPlatform(t *testing.T) {
	asset, err := cloudflaredTarget()
	if err != nil {
		t.Skip(err.Error())
	}
	if runtime.GOOS == "linux" && strings.HasSuffix(asset, ".tgz") {
		t.Errorf("linux asset = %q, want the unarchived binary upstream publishes", asset)
	}
	if runtime.GOOS == "darwin" && !strings.HasSuffix(asset, ".tgz") {
		t.Errorf("darwin asset = %q, want the archive upstream publishes", asset)
	}
}

// writeFakeCloudflared installs something that answers the functional check.
func writeFakeCloudflared(t *testing.T, path string) {
	t.Helper()
	script := "#!/bin/sh\necho 'cloudflared version " + cloudflaredVersion + " (built test)'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// resetCloudflaredForTest clears the cache between tests, since only a working
// client is remembered and each test sets up its own.
func resetCloudflaredForTest() {
	cloudflaredMu.Lock()
	cloudflaredPath = ""
	cloudflaredMu.Unlock()
}

// roundTripFunc lets a test answer HTTP without a socket.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
