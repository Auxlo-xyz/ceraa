// Package selfupdate downloads a newer Ceraa release and swaps it in over the
// running binary.
//
// The public repository carries no source, so an installed copy cannot build
// itself a new version and cannot ask a build system for one. The only two
// things that exist are the release API, which names the tag, and the release
// download, which serves the bytes plus the SHA256SUMS that say which bytes
// were published. install.sh performs the same three steps in shell; this
// package exists because the shell script installs a copy the running process
// does not have yet, and this one replaces the copy that is running.
//
// Replacing a running executable is safe on Linux and macOS only if the new
// file is written in the same directory and renamed over the old one. The
// kernel keeps mapping the old inode for the process that opened it, so the
// service is not disturbed: it goes on executing the version it started with
// until somebody restarts it. That is also the one part this package cannot do,
// which is why callers report the running service instead of implying the new
// version is already live.
//
// Nothing here runs automatically on a timer. The bridge only looks, on a
// bounded and backgrounded call, and the swap happens when somebody asks for it.
package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"ceraa/internal/build"
)

// Repo is the public binary distribution, and the only place a release exists.
const Repo = "Auxlo-xyz/ceraa"

// The two endpoints, as vars rather than consts so a test can point them at a
// local server. They are not settings: nothing in the product is configurable
// about where a release comes from, because a tool that decides at runtime what
// to download is a tool that can be made to download something else.
var (
	apiLatestURL    = "https://api.github.com/repos/" + Repo + "/releases/latest"
	releaseDownload = "https://github.com/" + Repo + "/releases/download/"
)

// fetchBudget bounds a whole update: the API call, the checksum file, the
// binary and the prompt. Generous enough for a large binary on a slow link and
// short enough that a hung endpoint is noticed while somebody is watching.
const fetchBudget = 2 * time.Minute

// Size caps. A release binary is under 40 MB and the prompt is 7 KB; anything
// vastly larger is not the artifact that was asked for, and an unbounded read
// is an unbounded allocation.
const (
	maxBinaryBytes = 64 << 20
	maxSumsBytes   = 1 << 20
)

// Status is what a check found.
type Status struct {
	Current   string
	Latest    string
	Available bool
}

// String renders the status as one line for a log or a terminal.
func (s Status) String() string {
	switch {
	case s.Latest == "":
		return "ceraa " + s.Current
	case s.Available:
		return fmt.Sprintf("ceraa %s -> %s is available", s.Current, s.Latest)
	default:
		return fmt.Sprintf("ceraa %s is current (latest %s)", s.Current, s.Latest)
	}
}

// Result is what an applied update did.
type Result struct {
	Tag      string
	Binary   string
	Probed   string
	Replaced bool
}

// Latest returns the newest release tag, for example "v0.5.0".
func Latest(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchBudget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiLatestURL, nil)
	if err != nil {
		return "", err
	}
	// Named, because the API answers a request it cannot identify with 403 and
	// a bare Go user agent is not an identification.
	req.Header.Set("User-Agent", "ceraa/"+build.Version)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("reach %s: %w", apiLatestURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("latest release: HTTP %d from the release API", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSumsBytes)).Decode(&body); err != nil {
		return "", fmt.Errorf("read the release API response: %w", err)
	}
	tag := strings.TrimSpace(body.TagName)
	if tag == "" {
		return "", errors.New("the release API named no tag; is there a release?")
	}
	return tag, nil
}

// Check compares the running version against the newest release.
//
// An error leaves Current filled in, because a caller that reports "could not
// reach" should still say what it is running.
func Check(ctx context.Context) (Status, error) {
	status := Status{Current: build.Version}
	tag, err := Latest(ctx)
	if err != nil {
		return status, err
	}
	status.Latest = tag
	status.Available = Newer(build.Version, tag)
	return status, nil
}

// Apply fetches tag (the newest release when empty), verifies it, checks that
// the candidate actually runs and reports the version the tag names, and only
// then renames it over the running binary.
//
// The probe is the same assertion the release workflow makes, and it is made
// here for the same reason: a binary that installs and never runs is the worst
// outcome available, and the only signal strong enough to catch it is asking the
// new file what it is.
func Apply(ctx context.Context, tag string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchBudget)
	defer cancel()

	var result Result
	asset, err := assetName()
	if err != nil {
		return result, err
	}
	if tag == "" {
		if tag, err = Latest(ctx); err != nil {
			return result, err
		}
	}
	if tag, err = normalizeTag(tag); err != nil {
		return result, err
	}
	result.Tag = tag

	target, err := targetBinary()
	if err != nil {
		return result, err
	}
	result.Binary = target

	base := releaseDownload + tag
	sums, err := fetch(ctx, base+"/SHA256SUMS", maxSumsBytes)
	if err != nil {
		return result, err
	}
	binary, err := fetch(ctx, base+"/"+asset, maxBinaryBytes)
	if err != nil {
		return result, err
	}
	if err := verify(sums, asset, binary); err != nil {
		return result, err
	}

	// The candidate is written beside the target and only renamed over it once
	// everything else has succeeded, so a failure anywhere below leaves the
	// installed binary untouched.
	candidate, err := stage(target, binary, 0o755)
	if err != nil {
		return result, err
	}
	committed := false
	defer func() {
		if !committed {
			os.Remove(candidate)
		}
	}()

	reported, err := runProbe(ctx, candidate)
	if err != nil {
		return result, fmt.Errorf("the downloaded binary did not run; nothing was replaced: %w", err)
	}
	want := "ceraa " + strings.TrimPrefix(tag, "v")
	if reported != want {
		return result, fmt.Errorf("the downloaded binary reports %q but the tag says %q; nothing was replaced", reported, want)
	}
	result.Probed = reported

	// The prompt is compiled into the candidate binary, so updating a binary is
	// atomic and cannot leave a mismatched sidecar behind.
	if err := os.Rename(candidate, target); err != nil {
		return result, fmt.Errorf("replace %s: %w", target, err)
	}
	committed = true
	result.Replaced = true
	return result, nil
}

// targetBinary is the file that gets replaced.
//
// A var so a test points it at a scratch file instead of the test binary, which
// would be unrecoverable: the test would replace itself.
var targetBinary = func() (string, error) {
	return os.Executable()
}

// runProbe asks a binary what version it is.
var runProbe = probe

// stage writes data to a fresh file beside path and returns its name.
//
// Beside, not in a temp directory elsewhere: a rename across filesystems is a
// copy, and the copy is the part that can fail halfway. /tmp and /usr/local are
// different filesystems on most machines.
func stage(path string, data []byte, mode os.FileMode) (string, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ceraa-update-*")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return "", fmt.Errorf("cannot write to %s: %w. Self-update replaces files in place, so the directory holding them has to be writable; reinstall with install.sh instead", dir, err)
		}
		return "", fmt.Errorf("create a file in %s: %w", dir, err)
	}
	name := tmp.Name()
	discard := true
	defer func() {
		if discard {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return "", fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	discard = false
	return name, nil
}

// replaceInPlace writes data beside path and renames it over path.
//
// The rename, not a write through the old file: a running process keeps the old
// inode mapped, so overwriting its contents in place would be rewriting memory
// that is still in use.
func replaceInPlace(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	// The prompt may be the first thing to land in a prompt/ directory that was
	// never created, which is what a hand install with a flat system.txt looks
	// like. CreateTemp does not make directories, so without this the refresh
	// fails with a bare ENOENT on an install that otherwise works.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	staged, err := stage(path, data, mode)
	if err != nil {
		return err
	}
	if err := os.Rename(staged, path); err != nil {
		os.Remove(staged)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// probe runs `path version` and returns its first line of stdout.
//
// stderr is discarded rather than merged: a missing prompt writes a warning to
// stderr and still exits zero, and merging it would make the version line fail
// to parse for a reason that has nothing to do with the version.
func probe(ctx context.Context, path string) (string, error) {
	cmd := exec.CommandContext(ctx, path, "version")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("run %s version: %w", path, err)
	}
	line := out.String()
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return strings.TrimSpace(line), nil
}

// Prompt is embedded in the binary and has no separate update result.
// statusError carries the HTTP status so a caller can tell a missing asset from
// a broken connection.
type statusError struct {
	url    string
	status int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("fetching %s: HTTP %d", e.url, e.status)
}

func fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ceraa/"+build.Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{url: url, status: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than the %d byte limit", url, limit)
	}
	return data, nil
}

// verify checks downloaded bytes against the published checksum.
func verify(sums []byte, name string, data []byte) error {
	want, err := checksumFor(sums, name)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return fmt.Errorf("checksum mismatch for %s\n  expected %s\n  got      %s\nThe download is corrupt or was tampered with, so nothing was replaced.", name, want, got)
	}
	return nil
}

// checksumFor finds the digest published for exactly this filename.
//
// Exactly, and not by suffix: install.sh anchors the same match for the same
// reason. A substring match would accept "ceraa-linux-amd64-notreally" as a
// match for "ceraa-linux-amd64", and would let a line for some other file
// stand in for this one.
func checksumFor(sums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// sha256sum writes a leading "*" for binary mode.
		if strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		digest := strings.ToLower(fields[0])
		if _, err := hex.DecodeString(digest); err != nil || len(digest) != 64 {
			return "", fmt.Errorf("the checksum published for %s is not a sha256 digest", name)
		}
		return digest, nil
	}
	return "", fmt.Errorf("no checksum for %s in SHA256SUMS", name)
}

// assetName is the release filename for this machine.
//
// The names are fixed by the release workflow rather than derived, so an
// unsupported platform says so instead of asking for a file that was never
// published and reporting a 404.
func assetName() (string, error) {
	switch runtime.GOOS {
	case "linux", "darwin":
	default:
		return "", fmt.Errorf("self-update ships linux and darwin only, and this is %s; reinstall with install.sh", runtime.GOOS)
	}
	switch runtime.GOARCH {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("self-update ships amd64 and arm64 only, and this is %s; reinstall with install.sh", runtime.GOARCH)
	}
	return "ceraa-" + runtime.GOOS + "-" + runtime.GOARCH, nil
}

// tagRe accepts the shape a tag is allowed to have.
//
// The tag is concatenated into a URL, so anything that could change which host
// or path is contacted has to be rejected before it gets there. It is also the
// string compared against the running version, so a tag that is not a version
// would compare as garbage.
var tagRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)

// normalizeTag validates a tag and returns it in the canonical v-prefixed form.
func normalizeTag(tag string) (string, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return "", errors.New("empty release tag")
	}
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	if !tagRe.MatchString(tag) {
		return "", fmt.Errorf("%q is not a release tag like v0.5.0", tag)
	}
	return tag, nil
}

// Newer reports whether current is behind latest.
func Newer(current, latest string) bool {
	return Compare(current, latest) < 0
}

// Compare orders two version strings.
//
// It understands the shapes the product actually produces: a release injected
// from a tag ("0.5.0"), a development build ("0.4.6-dev"), and a tag itself
// ("v0.5.0"). A prerelease sorts before the release it precedes, which is what
// makes a -dev build notice its own release.
//
// An unparseable value sorts after everything, so a version nobody understands
// is treated as newer rather than older. Guessing the opposite would silently
// tell somebody to downgrade.
func Compare(a, b string) int {
	ca, preA, okA := parseVersion(a)
	cb, preB, okB := parseVersion(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return 1
	case !okB:
		return -1
	}
	width := len(ca)
	if len(cb) > width {
		width = len(cb)
	}
	for i := 0; i < width; i++ {
		x, y := 0, 0
		if i < len(ca) {
			x = ca[i]
		}
		if i < len(cb) {
			y = cb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case preA == "" && preB == "":
		return 0
	case preA == "":
		return 1
	case preB == "":
		return -1
	}
	return strings.Compare(preA, preB)
}

// parseVersion splits a version into its numeric parts and its prerelease.
func parseVersion(value string) ([]int, string, bool) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "v")
	if value == "" {
		return nil, "", false
	}
	// A build metadata suffix (+anything) is not part of the ordering, and it
	// follows the prerelease rather than living inside it, so it is cut first:
	// a hyphen inside the metadata is not a prerelease marker.
	if i := strings.IndexByte(value, '+'); i >= 0 {
		value = value[:i]
	}
	var pre string
	if i := strings.IndexByte(value, '-'); i >= 0 {
		pre = value[i+1:]
		value = value[:i]
	}
	parts := strings.Split(value, ".")
	core := make([]int, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, "", false
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, "", false
		}
		core = append(core, n)
	}
	return core, pre, true
}
