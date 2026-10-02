package provision

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// The release to fetch. Pinned for the reason ripgrep's is: a tool that decides
// at runtime what to download is a tool that can be made to download something
// else, and at this size that something is large.
const cloudflaredVersion = "2026.9.3"

const cloudflaredRelease = "https://github.com/cloudflare/cloudflared/releases/download/" + cloudflaredVersion

// cloudflaredBudget bounds the whole fetch. Longer than ripgrep's because the
// artefact is an order of magnitude larger, and safe to be longer: this runs
// when somebody asks for a tunnel rather than on the way to answering a
// message, and the caller has already said one is opening.
const cloudflaredBudget = 3 * time.Minute

// maxCloudflaredBytes caps what is read out of the response. The published
// binary is under 40 MB and its archive is a third of that; anything vastly
// larger is not what was asked for, and an unbounded reader is an unbounded
// allocation.
const maxCloudflaredBytes = 96 << 20

// cloudflaredMu guards the resolved path. Only a working client is remembered:
// caching a failure would mean one unreachable release endpoint turns /web off
// for the rest of the process, and asking again is the whole point of a command.
var (
	cloudflaredMu   sync.Mutex
	cloudflaredPath string
)

// EnsureCloudflared returns the path of a tunnel client, and a line worth
// logging when there is one.
//
// The line is empty when the client was already available, because "found where
// you put it" is not news. It is filled in when a copy was fetched, so an
// operator can see why the first request took a while, and when the path is
// empty it carries the reason for the failure instead.
//
// The precedence is ripgrep's, for the same reasons:
//
//  1. Whatever is already on PATH wins. A package, a user's own build, a
//     Homebrew install. Nothing here overrides a working tool.
//  2. Otherwise a copy this process placed in Ceraa's bin directory, which is
//     the same one ripgrep uses, so uninstalling is deleting one folder.
//  3. Otherwise download the pinned release and install it there.
//
// Deliberately not wired into Report, which runs at startup for ripgrep. The
// difference is size and frequency: fetching tens of megabytes on every boot to
// serve a feature most processes never open would tax everybody for the few who
// share a tunnel. This runs the first time one is asked for, and the line it
// returns is the operator's receipt.
func EnsureCloudflared() (string, string) {
	cloudflaredMu.Lock()
	defer cloudflaredMu.Unlock()
	if cloudflaredPath != "" {
		return cloudflaredPath, ""
	}
	path, note := ensureCloudflared()
	if path != "" {
		cloudflaredPath = path
	}
	return path, note
}

func ensureCloudflared() (string, string) {
	if path, err := exec.LookPath("cloudflared"); err == nil && isCloudflared(path) {
		return path, ""
	}
	local := filepath.Join(binDir(), "cloudflared")
	if isCloudflared(local) {
		return local, ""
	}
	if err := fetchCloudflared(local); err != nil {
		return "", fmt.Sprintf("could not provision cloudflared (%v); install it yourself or set CERAA_CLOUDFLARED", err)
	}
	if !isCloudflared(local) {
		return "", "the downloaded file is not a working cloudflared"
	}
	return local, "provisioned cloudflared " + cloudflaredVersion + " into " + filepath.Dir(local)
}

// isCloudflared is a functional check rather than a file test, because a
// truncated 38 MB download passes an existence check and then fails with an
// unexplained exit from a client nobody ran.
func isCloudflared(path string) bool {
	if path == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	return err == nil && strings.Contains(string(out), "cloudflared")
}

// cloudflaredTarget maps this build to a published asset.
func cloudflaredTarget() (string, error) {
	return cloudflaredTargetFor(runtime.GOOS, runtime.GOARCH)
}

// cloudflaredTargetFor takes the platform as arguments so the whole table can
// be checked rather than only the machine the tests happen to run on.
//
// Linux ships the binary unarchived while macOS ships a tarball, so the two
// families are fetched differently rather than forced through one archive path
// that exists for only one of them. Windows has a published asset and is left
// out deliberately, matching ripgrep: nothing else in this tree installs one.
func cloudflaredTargetFor(goos, goarch string) (string, error) {
	switch goos {
	case "linux":
		switch goarch {
		case "amd64":
			return "cloudflared-linux-amd64", nil
		case "arm64":
			return "cloudflared-linux-arm64", nil
		case "386":
			return "cloudflared-linux-386", nil
		case "arm":
			return "cloudflared-linux-arm", nil
		}
	case "darwin":
		switch goarch {
		case "amd64":
			return "cloudflared-darwin-amd64.tgz", nil
		case "arm64":
			return "cloudflared-darwin-arm64.tgz", nil
		}
	}
	return "", fmt.Errorf("no published cloudflared build for %s/%s", goos, goarch)
}

// fetchCloudflared downloads the pinned release into dest.
//
// It is written beside the destination and renamed into place, which ripgrep's
// fetch does not bother with. A 38 MB transfer interrupted halfway would
// otherwise leave a truncated file at the path the next attempt trusts, and the
// functional check would then have to reject what looks like a finished install.
func fetchCloudflared(dest string) error {
	asset, err := cloudflaredTarget()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloudflaredBudget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cloudflaredRelease+"/"+asset, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: HTTP %d", asset, resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	partial := dest + ".part"
	out, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if strings.HasSuffix(asset, ".tgz") {
		err = extractCloudflared(resp.Body, out)
	} else {
		_, err = io.Copy(out, io.LimitReader(resp.Body, maxCloudflaredBytes))
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(partial)
		return err
	}
	return os.Rename(partial, dest)
}

// extractCloudflared copies the one binary out of the macOS archive.
//
// The archive's own entry name is never used as a path: everything is written
// to the destination the caller already chose, so an entry named ../../bin/sh
// has nowhere to go. That is why the traversal guard ripgrep needs does not
// appear here.
func extractCloudflared(r io.Reader, out io.Writer) error {
	gz, err := gzip.NewReader(io.LimitReader(r, maxCloudflaredBytes))
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != "cloudflared" {
			continue
		}
		_, err = io.Copy(out, io.LimitReader(reader, maxCloudflaredBytes))
		return err
	}
	return fmt.Errorf("no cloudflared binary inside the archive")
}
