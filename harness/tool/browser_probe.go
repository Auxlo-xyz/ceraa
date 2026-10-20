package tool

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/go-rod/rod/lib/launcher"
)

// existingBrowserBinary reports a browser this host already has, without
// downloading one. It is the probe's half of resolveBrowserBinary: doctor has to
// answer "can the browser start here" without spending the download a first-use
// launch would.
func existingBrowserBinary() (string, bool) {
	for _, candidate := range systemBrowserCandidates {
		if path, ok := executableExists(candidate); ok {
			return path, true
		}
	}
	// The downloader names the exact binary per platform (chrome on Linux,
	// Chromium.app/Contents/MacOS/Chromium on macOS), so ask it rather than
	// guessing a layout that would miss the darwin build.
	fetcher := launcher.NewBrowser()
	fetcher.RootDir = browserDownloadDir()
	if path, ok := executableExists(fetcher.BinPath()); ok {
		return path, true
	}
	return "", false
}

// ProbeBrowser reports whether an installed browser can start with the same
// library environment the tool gives it. It never downloads, so doctor can run
// it on every host.
//
// A host with no browser is not a failure: the first browser_open fetches one,
// and no browser at doctor time says nothing about whether that fetch will
// work.
func ProbeBrowser(ctx context.Context) (string, error) {
	binary, ok := existingBrowserBinary()
	if !ok {
		return "not installed yet; the first browser_open downloads one", nil
	}
	return probeBrowserBinary(ctx, binary)
}

// probeBrowserBinary asks a browser to print its version under the launch
// environment, which exercises the loader without opening a window. Splitting it
// from ProbeBrowser is what lets the failure be tested with a script rather
// than a 400 MB binary.
func probeBrowserBinary(ctx context.Context, binary string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = browserLaunchEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			err = fmt.Errorf("%w: %s", err, detail)
		}
		return "", fmt.Errorf("%s cannot start: %w%s", filepath.Base(binary), err, browserLibraryHint(err))
	}
	version := strings.TrimSpace(string(out))
	if i := strings.IndexByte(version, '\n'); i >= 0 {
		version = strings.TrimSpace(version[:i])
	}
	if version == "" {
		version = "version reported no output"
	}
	return version + " at " + binary, nil
}
