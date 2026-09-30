// Package provision makes a small set of external tools available to the agent,
// user-locally, without root and without touching system directories.
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

// The release to fetch. Pinned rather than "latest", because a tool that decides
// at runtime what to download is a tool that can be made to download something
// else, and a provisioning step should not have that property.
const ripgrepVersion = "14.1.1"

const releaseBase = "https://github.com/BurntSushi/ripgrep/releases/download/" + ripgrepVersion

// downloadBudget bounds the whole fetch. Provisioning happens on the way to
// answering, so a slow or hanging mirror must not become a slow or hanging
// start. Failing here costs a slower search, which the prompt already handles.
const downloadBudget = 90 * time.Second

// maxArchiveBytes caps what is read out of the response. A tarball for this tool
// is a couple of megabytes; anything vastly larger is not what was asked for, and
// an unbounded reader is an unbounded allocation.
const maxArchiveBytes = 64 << 20

var (
	once      sync.Once
	available bool
	reason    string
)

// EnsureRipgrep makes ripgrep reachable and reports whether it is.
//
// Precedence, and the order is the design:
//
//  1. Whatever is already on PATH wins. A distribution's package, a user's own
//     build, a Homebrew install. Nothing here overrides a working tool.
//  2. Otherwise a copy this process placed under Ceraa's data directory, which
//     makes it survive a restart without a second download.
//  3. Otherwise download the pinned release and install it there.
//
// It is deliberately user-local. Installing into a system package directory
// needs root, fights the distribution's own package, and is the kind of thing
// that should be a user's decision rather than a side effect of starting an
// agent. Everything lands under one directory that a user can delete to
// uninstall, and nothing outside Ceraa's own data directory is ever written.
//
// Runs once per process. Concurrent callers block on the first call rather than
// racing two downloads into the same path.
func EnsureRipgrep() bool {
	once.Do(func() { available, reason = ensure() })
	return available
}

// ProvisionReason explains the last outcome, for a log line. Empty when
// ripgrep was found without any provisioning.
func ProvisionReason() string {
	once.Do(func() { available, reason = ensure() })
	return reason
}

func ensure() (bool, string) {
	if path, err := exec.LookPath("rg"); err == nil {
		return true, "found on PATH at " + path
	}
	dir := binDir()
	local := filepath.Join(dir, "rg")
	if isRipgrep(local) {
		addToPath(dir)
		return true, "using the copy in " + dir
	}
	target, err := releaseTarget()
	if err != nil {
		return false, err.Error()
	}
	if err := download(target, dir); err != nil {
		// Not fatal and not noisy. The prompt names grep instead, which is
		// slower and works. An agent that refuses to start because a search
		// tool could not be fetched is worse than one that searches more slowly.
		return false, fmt.Sprintf("could not provision ripgrep (%v); falling back to grep", err)
	}
	if !isRipgrep(local) {
		return false, "the downloaded file is not a working ripgrep; falling back to grep"
	}
	addToPath(dir)
	return true, "provisioned ripgrep " + ripgrepVersion + " into " + dir
}

// binDir keeps provisioned tools with the rest of Ceraa's data, so uninstalling
// is deleting one directory and the footprint is visible rather than scattered.
func binDir() string {
	dir := filepath.Join(dataDir(), "bin")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func dataDir() string {
	if override := os.Getenv("CERAA_HOME"); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return filepath.Join(home, ".config", "ceraa")
}

// isRipgrep is a functional check rather than a file test: a truncated download
// and a wrong binary both pass an existence check, and the failure would then be
// a confusing "rg: not found" from inside a shell much later.
func isRipgrep(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	return err == nil && strings.Contains(string(out), "ripgrep")
}

// addToPath puts the provisioned directory ahead of PATH, so a later shell
// resolves rg to this copy.
//
// The process environment rather than a per-call environment, because the shell
// tool runs `bash -c` and inherits it, and doing it once at startup is cheaper
// and less surprising than threading it through every call site.
func addToPath(dir string) {
	current := os.Getenv("PATH")
	if current == "" {
		_ = os.Setenv("PATH", dir)
		return
	}
	for _, entry := range filepath.SplitList(current) {
		if entry == dir {
			return
		}
	}
	_ = os.Setenv("PATH", dir+string(os.PathListSeparator)+current)
}

// releaseTarget maps this build to a published ripgrep asset.
//
// musl is chosen deliberately for Linux amd64: it is a static build, so it runs
// on a distribution with a different libc, which is the same reason the Go
// binary is built with CGO disabled.
func releaseTarget() (string, error) {
	arch, ok := map[string]string{
		"amd64": "x86_64",
		"arm64": "aarch64",
		"386":   "i686",
		"arm":   "armv7",
	}[runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("no published ripgrep build for %s", runtime.GOARCH)
	}
	var target string
	switch runtime.GOOS {
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			target = "x86_64-unknown-linux-musl"
		case "arm64":
			target = "aarch64-unknown-linux-gnu"
		case "386":
			target = "i686-unknown-linux-musl"
		case "arm":
			target = "armv7-unknown-linux-gnueabihf"
		}
	case "darwin":
		switch runtime.GOARCH {
		case "amd64":
			target = "x86_64-apple-darwin"
		case "arm64":
			target = "aarch64-apple-darwin"
		}
	}
	if target == "" {
		return "", fmt.Errorf("no published ripgrep build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	_ = arch
	return fmt.Sprintf("ripgrep-%s-%s.tar.gz", ripgrepVersion, target), nil
}

// download fetches the pinned release and writes the single binary it contains.
//
// Entries are rejected unless they resolve inside the destination directory. A
// tarball is untrusted input and an entry named "../../bin/sh" would otherwise
// write outside it.
func download(asset, dir string) error {
	target, err := releaseTarget()
	if err != nil {
		return err
	}
	_ = asset
	ctx, cancel := context.WithTimeout(context.Background(), downloadBudget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseBase+"/"+target, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: HTTP %d", target, resp.StatusCode)
	}

	gz, err := gzip.NewReader(io.LimitReader(resp.Body, maxArchiveBytes))
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	destination, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != "rg" {
			continue
		}
		name := filepath.Join(dir, "rg")
		if !withinDir(destination, name) {
			return fmt.Errorf("archive entry %q resolves outside the destination", header.Name)
		}
		// 0o755, and the file is created rather than opened for append, so a
		// leftover from a previous run cannot be appended to.
		out, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, io.LimitReader(reader, maxArchiveBytes)); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
	return fmt.Errorf("no ripgrep binary inside %s", target)
}

func withinDir(dir, name string) bool {
	rel, err := filepath.Rel(dir, name)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
