package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// digest is hex(sha256(data)), the form SHA256SUMS is written in.
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// releaseLine is one row of SHA256SUMS, including the two spaces sha256sum
// writes between the digest and the name.
func releaseLine(name string, data []byte) string {
	return digest(data) + "  " + name + "\n"
}

// TestCompareOrdersVersions is the ordering the whole feature rests on: get it
// backwards and a working install is told to downgrade.
func TestCompareOrdersVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.4.6", "0.5.0", -1},
		{"0.5.0", "0.4.6", 1},
		{"0.5.0", "v0.5.0", 0},
		{"0.4.6-dev", "0.4.6", -1},
		{"0.4.6-dev", "v0.5.0", -1},
		{"v0.5.0", "0.4.6-dev", 1},
		// Numeric, not lexicographic: 0.10.0 is after 0.9.0.
		{"0.10.0", "0.9.0", 1},
		{"0.9.0", "0.10.0", -1},
		{"0.5.0", "0.5.0", 0},
		// A release sorts after its own release candidate.
		{"1.0.0", "1.0.0-rc.1", 1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0-rc.1", "1.0.0-rc.2", -1},
		// Missing parts compare as zero rather than as a shorter version.
		{"1.0", "1.0.0", 0},
		// Build metadata never affects the order, even when it contains a
		// hyphen that would otherwise read as a prerelease.
		{"0.9.2+workbuddy-polish", "v0.9.2", 0},
		{"0.9.2+build.7", "0.9.2", 0},
		{"1.0.0-alpha+sha.123", "1.0.0-alpha", 0},
		{"1.0.0-alpha+sha.123", "1.0.0", -1},
	}
	for _, tc := range cases {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// An unparseable version sorts last, so it is treated as current rather than as
// something to move away from. Guessing the other way would tell somebody to
// downgrade off a string nobody understands.
func TestCompareTreatsAnUnknownVersionAsNewest(t *testing.T) {
	if got := Compare("", "0.1.0"); got <= 0 {
		t.Errorf("Compare(\"\", \"0.1.0\") = %d, want a value above 0", got)
	}
	if got := Compare("garbage", "0.1.0"); got <= 0 {
		t.Errorf("Compare(\"garbage\", \"0.1.0\") = %d, want a value above 0", got)
	}
	if got := Compare("", "garbage"); got != 0 {
		t.Errorf("Compare(\"\", \"garbage\") = %d, want 0 (neither parses)", got)
	}
}

// The shape a development build reports is the one a developer actually runs,
// so it has to see its own release as an update.
func TestNewerSeesADevBuildAsBehind(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"0.4.6-dev", "v0.5.0", true},
		{"0.4.6", "v0.5.0", true},
		{"0.5.0-dev", "v0.5.0", true},
		{"0.5.0", "v0.5.0", false},
		{"0.5.1", "v0.5.0", false},
		{"0.9.2+workbuddy-polish", "v0.9.2", false},
		{"0.0.0-dev", "v0.5.0", true},
	}
	for _, tc := range cases {
		if got := Newer(tc.current, tc.latest); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

// The filename is matched exactly and never by prefix. install.sh anchors the
// same match for the same reason: a line for a different file must not be able
// to stand in for this one, or a substituted asset would pass its own check.
func TestChecksumForMatchesTheNameExactly(t *testing.T) {
	decoy := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	real := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// Decoy first on purpose: a prefix match would return it.
	sums := []byte(decoy + "  ceraa-linux-amd64-notreally\n" + real + "  ceraa-linux-amd64\n")

	got, err := checksumFor(sums, "ceraa-linux-amd64")
	if err != nil {
		t.Fatalf("checksumFor: %v", err)
	}
	if got != real {
		t.Errorf("checksumFor returned %s, want the digest for the exact name %s", got, real)
	}
}

// sha256sum writes a leading "*" when asked for a binary-mode checksum, and a
// release can be produced either way.
func TestChecksumForAcceptsTheBinaryModeMarker(t *testing.T) {
	want := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	got, err := checksumFor([]byte(want+"  *ceraa-linux-amd64\n"), "ceraa-linux-amd64")
	if err != nil {
		t.Fatalf("checksumFor: %v", err)
	}
	if got != want {
		t.Errorf("checksumFor = %s, want %s", got, want)
	}
}

func TestChecksumForRejectsWhatIsNotPublished(t *testing.T) {
	sums := []byte("dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd  ceraa-linux-amd64\n")
	if _, err := checksumFor(sums, "system.txt"); err == nil {
		t.Error("a name that is not in SHA256SUMS was accepted")
	}
	// A digest of the wrong length is not something to trust with an executable.
	bad := []byte("dead  ceraa-linux-amd64\n")
	if _, err := checksumFor(bad, "ceraa-linux-amd64"); err == nil {
		t.Error("a digest that is not a sha256 was accepted")
	}
}

// The tag is concatenated into a URL, so what it may contain is not a style
// question.
func TestNormalizeTagOnlyAcceptsAVersion(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"v0.5.0", "v0.5.0", false},
		{"0.5.0", "v0.5.0", false},
		{" v0.5.0 ", "v0.5.0", false},
		{"v0.5.0-rc.1", "v0.5.0-rc.1", false},
		{"", "", true},
		{"latest", "", true},
		{"v0.5", "", true},
		{"v0.5.0/../../etc/passwd", "", true},
		{"v0.5.0%2f..", "", true},
		{"http://evil.example/v0.5.0", "", true},
	}
	for _, tc := range cases {
		got, err := normalizeTag(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeTag(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeTag(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeTag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The loader no longer reads disk prompt files because the prompt is embedded.
// Self-update therefore never needs a prompt target.

func TestAssetNameIsWhatTheReleasePublishes(t *testing.T) {
	name, err := assetName()
	if err != nil {
		t.Fatalf("assetName on this platform: %v", err)
	}
	// The release workflow publishes exactly these four names, so anything else
	// would be a file nobody ever uploaded.
	valid := map[string]bool{
		"ceraa-linux-amd64":  true,
		"ceraa-linux-arm64":  true,
		"ceraa-darwin-amd64": true,
		"ceraa-darwin-arm64": true,
	}
	if !valid[name] {
		t.Errorf("assetName = %q, which is not one of the published release assets", name)
	}
}

// A downloaded file that has to run before anything is replaced means the
// probe itself must work on a plain file, not only on a Go binary. This is a
// shell script standing in for the release.
func TestProbeReadsTheFirstLineOfStdout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ceraa")
	script := "#!/bin/sh\n# a warning would go to stderr\necho 'ceraa 0.5.0'\necho extra\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got != "ceraa 0.5.0" {
		t.Errorf("probe = %q, want %q", got, "ceraa 0.5.0")
	}
}

// stubRelease points the package at a local server standing in for the release.
//
// The endpoints are package vars precisely so this is possible: nothing here
// talks to GitHub in a unit test, and nothing here can be configured by a user
// either.
func stubRelease(t *testing.T, tag string, files map[string][]byte) *httptest.Server {
	t.Helper()
	asset, err := assetName()
	if err != nil {
		t.Fatalf("assetName: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q}`, tag)
	})
	for name, data := range files {
		if name == "SHA256SUMS" || name == asset {
			continue
		}
		path := "/download/" + tag + "/" + name
		body := data
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Write(body)
		})
	}
	mux.HandleFunc("/download/"+tag+"/"+asset, func(w http.ResponseWriter, r *http.Request) {
		w.Write(files[asset])
	})
	mux.HandleFunc("/download/"+tag+"/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		w.Write(files["SHA256SUMS"])
	})
	srv := httptest.NewServer(mux)

	oldLatest, oldDownload := apiLatestURL, releaseDownload
	apiLatestURL = srv.URL + "/latest"
	releaseDownload = srv.URL + "/download/"
	t.Cleanup(func() {
		apiLatestURL, releaseDownload = oldLatest, oldDownload
		srv.Close()
	})
	return srv
}

// stubTarget installs a scratch file as the thing that gets replaced. Without
// this the update would replace the test binary itself, and the test would never
// get to report anything.
func stubTarget(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ceraa")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'ceraa 0.4.6'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := targetBinary
	targetBinary = func() (string, error) { return path, nil }
	t.Cleanup(func() { targetBinary = old })
	return dir, path
}

func TestApplyReplacesTheBinary(t *testing.T) {
	dir, target := stubTarget(t)

	asset, _ := assetName()
	updated := []byte("#!/bin/sh\necho 'ceraa 0.5.0'\n")
	sums := []byte(releaseLine(asset, updated))
	stubRelease(t, "v0.5.0", map[string][]byte{
		asset:        updated,
		"SHA256SUMS": sums,
	})

	result, err := Apply(context.Background(), "")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Replaced {
		t.Error("Apply reported no replacement")
	}
	if result.Tag != "v0.5.0" {
		t.Errorf("Tag = %q, want v0.5.0", result.Tag)
	}
	if result.Probed != "ceraa 0.5.0" {
		t.Errorf("Probed = %q, want %q", result.Probed, "ceraa 0.5.0")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read the replaced binary: %v", err)
	}
	if string(got) != string(updated) {
		t.Errorf("the binary was not replaced with the release bytes")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the replaced binary is not executable: %v", info.Mode())
	}

	// Prompt is embedded in the replacement binary, so no sidecar is written.
	if _, err := os.Stat(filepath.Join(dir, "prompt", "system.txt")); err == nil {
		t.Error("a prompt sidecar was written for an embedded prompt")
	}
	// The staged candidate must not be left behind next to the binary.
	leftovers, err := filepath.Glob(filepath.Join(dir, ".ceraa-update-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) > 0 {
		t.Errorf("staged files were left behind: %v", leftovers)
	}
}

// The probe is the last gate before a rename that cannot be undone by this
// process. A candidate that starts but is not the release the tag names must
// cost the whole update.
func TestApplyLeavesTheInstallAloneWhenTheCandidateIsNotTheRelease(t *testing.T) {
	_, target := stubTarget(t)
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	asset, _ := assetName()
	// Runs, and reports a version the tag does not name.
	wrong := []byte("#!/bin/sh\necho 'ceraa 9.9.9'\n")
	stubRelease(t, "v0.5.0", map[string][]byte{
		asset:        wrong,
		"SHA256SUMS": []byte(releaseLine(asset, wrong)),
	})

	if _, err := Apply(context.Background(), ""); err == nil {
		t.Fatal("Apply accepted a candidate that reports the wrong version")
	} else if !strings.Contains(err.Error(), "nothing was replaced") {
		t.Errorf("error does not say the install was left alone: %v", err)
	}

	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("the installed binary changed even though the update was refused")
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".ceraa-update-*"))
	if len(leftovers) > 0 {
		t.Errorf("a refused update left staged files behind: %v", leftovers)
	}
}

// A checksum mismatch is a corrupt or substituted download, and it has to be
// caught before the bytes are made executable anywhere.
func TestApplyRefusesAChecksumMismatch(t *testing.T) {
	_, target := stubTarget(t)
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	asset, _ := assetName()
	payload := []byte("#!/bin/sh\necho 'ceraa 0.5.0'\n")
	sums := []byte(digest([]byte("something else")) + "  " + asset + "\n")
	stubRelease(t, "v0.5.0", map[string][]byte{
		asset:        payload,
		"SHA256SUMS": sums,
	})

	if _, err := Apply(context.Background(), ""); err == nil {
		t.Fatal("Apply accepted a checksum mismatch")
	} else if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error does not name the mismatch: %v", err)
	}

	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("the installed binary changed despite a checksum mismatch")
	}
}

// A tag that is not a version never reaches the network.
func TestApplyRejectsATagThatIsNotAVersion(t *testing.T) {
	_, target := stubTarget(t)
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), "../../etc/passwd"); err == nil {
		t.Fatal("Apply accepted a tag that is not a version")
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("the installed binary changed")
	}
}

// A release no longer carries a prompt sidecar; the binary is self-contained.
func TestApplyDoesNotCreatePromptSidecar(t *testing.T) {
	_, target := stubTarget(t)
	asset, _ := assetName()
	updated := []byte("#!/bin/sh\necho 'ceraa 0.5.0'\n")
	stubRelease(t, "v0.5.0", map[string][]byte{
		asset:        updated,
		"SHA256SUMS": []byte(releaseLine(asset, updated)),
	})

	if _, err := Apply(context.Background(), ""); err != nil {
		t.Fatalf("Apply without a prompt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(target), "prompt", "system.txt")); err == nil {
		t.Error("a prompt sidecar was written for an embedded prompt release")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the binary was not replaced: %v", err)
	}
}

func TestStatusSaysWhatMatters(t *testing.T) {
	current := Status{Current: "0.4.6", Latest: "v0.5.0", Available: true}
	if got := current.String(); !strings.Contains(got, "0.4.6 -> v0.5.0") {
		t.Errorf("available status = %q", got)
	}
	quiet := Status{Current: "0.5.0", Latest: "v0.5.0"}
	if got := quiet.String(); !strings.Contains(got, "current") {
		t.Errorf("current status = %q", got)
	}
}
