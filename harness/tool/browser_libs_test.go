package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The browser used to depend on LD_LIBRARY_PATH being set in whatever shell
// started the daemon. Nothing recorded that, so any restart without it broke
// the tool with a loader error that looked like a missing library rather than a
// missing instruction. These pin the resolution to code.

// isolateBrowserLibs keeps a library directory this machine really has (the
// managed one beside the downloaded browser) out of an expectation about some
// other directory. Without it the tests were reading the host.
func isolateBrowserLibs(t *testing.T) {
	t.Helper()
	t.Setenv("CERAA_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

func TestBrowserLibraryPathFindsAKnownLocalDirectory(t *testing.T) {
	isolateBrowserLibs(t)
	dir := t.TempDir()
	t.Setenv("CERAA_BROWSER_LD_LIBRARY_PATH", "")
	t.Setenv("LD_LIBRARY_PATH", "")

	// Point the candidate list at a directory that exists for the duration of
	// the test, so this does not depend on whether this machine has one.
	real := browserSharedLibDirs
	t.Cleanup(func() { browserSharedLibDirs = real })
	browserSharedLibDirs = []string{dir}

	got := browserLibraryPath()
	if got != dir {
		t.Errorf("browserLibraryPath() = %q, want %q", got, dir)
	}
	if !strings.Contains(browserLaunchEnvValue(), "LD_LIBRARY_PATH="+dir) {
		t.Error("the launch environment does not carry the library path")
	}
	browserSharedLibDirs = real
}

func TestTheConfiguredPathIsSearchedBeforeTheFallback(t *testing.T) {
	isolateBrowserLibs(t)
	configured := t.TempDir()
	fallback := t.TempDir()
	t.Setenv("CERAA_BROWSER_LD_LIBRARY_PATH", configured)
	t.Setenv("LD_LIBRARY_PATH", "")

	real := browserSharedLibDirs
	t.Cleanup(func() { browserSharedLibDirs = real })
	browserSharedLibDirs = []string{fallback}

	got := browserLibraryPath()
	// Order decides which directory the loader reaches for first, so the
	// operator's setting has to lead.
	if !strings.HasPrefix(got, configured) {
		t.Errorf("browserLibraryPath() = %q, want it to start with the configured %q", got, configured)
	}
	if !strings.Contains(got, fallback) {
		t.Errorf("browserLibraryPath() = %q, want it to keep the fallback %q", got, fallback)
	}
	browserSharedLibDirs = real
}

func TestBrowserLibraryPathIgnoresDirectoriesThatAreNotThere(t *testing.T) {
	isolateBrowserLibs(t)
	t.Setenv("CERAA_BROWSER_LD_LIBRARY_PATH", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("LD_LIBRARY_PATH", "")

	real := browserSharedLibDirs
	t.Cleanup(func() { browserSharedLibDirs = real })
	browserSharedLibDirs = nil

	if got := browserLibraryPath(); got != "" {
		t.Errorf("browserLibraryPath() = %q, want empty when nothing exists", got)
	}
	browserSharedLibDirs = real
}

// The ambient path is kept, not replaced. A host that already works by exporting
// the variable must keep working, and a stale entry is harmless because the
// loader just skips a directory that lacks the name.
func TestBrowserLibraryPathKeepsTheAmbientPath(t *testing.T) {
	isolateBrowserLibs(t)
	existing := t.TempDir()
	t.Setenv("CERAA_BROWSER_LD_LIBRARY_PATH", "")
	t.Setenv("LD_LIBRARY_PATH", existing)

	real := browserSharedLibDirs
	t.Cleanup(func() { browserSharedLibDirs = real })
	browserSharedLibDirs = nil

	if got := browserLibraryPath(); got != existing {
		t.Errorf("browserLibraryPath() = %q, want the ambient %q", got, existing)
	}
	browserSharedLibDirs = real
}

func TestBrowserLibraryPathDoesNotRepeatADirectory(t *testing.T) {
	isolateBrowserLibs(t)
	dir := t.TempDir()
	t.Setenv("CERAA_BROWSER_LD_LIBRARY_PATH", dir)
	t.Setenv("LD_LIBRARY_PATH", dir)

	real := browserSharedLibDirs
	t.Cleanup(func() { browserSharedLibDirs = real })
	browserSharedLibDirs = []string{dir}

	if got := browserLibraryPath(); got != dir {
		t.Errorf("browserLibraryPath() = %q, want the directory once", got)
	}
	browserSharedLibDirs = real
}

// A loader failure is the one error an operator cannot act on unaided, so it
// has to name the fix. Any other failure is left alone.
func TestBrowserLibraryHintOnlyFiresOnALoaderFailure(t *testing.T) {
	missing := &os.PathError{Op: "fork/exec", Path: "/x/chrome",
		Err: errString("error while loading shared libraries: libatk-1.0.so.0: cannot open shared object file")}
	hint := browserLibraryHint(missing)
	if !strings.Contains(hint, "CERAA_BROWSER_LD_LIBRARY_PATH") {
		t.Errorf("hint = %q, want it to name the variable that fixes this", hint)
	}
	if !strings.Contains(hint, filepath.Join("browser", "libs")) {
		t.Errorf("hint = %q, want it to name the managed directory the launch path already reads", hint)
	}
	if got := browserLibraryHint(errString("connection refused")); got != "" {
		t.Errorf("hint = %q, want none for an unrelated failure", got)
	}
}

// A host-neutral location, so another machine can unpack the libraries there
// and be found without an environment variable. The absolute path this list used
// to name only existed on the machine it was written on.
func TestBrowserLibraryPathFindsTheHomeLocalLibs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CERAA_HOME", t.TempDir())
	t.Setenv("CERAA_BROWSER_LD_LIBRARY_PATH", "")
	t.Setenv("LD_LIBRARY_PATH", "")

	real := browserSharedLibDirs
	t.Cleanup(func() { browserSharedLibDirs = real })
	browserSharedLibDirs = nil

	dir := filepath.Join(home, ".local", "chrome-libs", "root", "usr", "lib", "x86_64-linux-gnu")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := browserLibraryPath(); !strings.Contains(got, dir) {
		t.Errorf("browserLibraryPath() = %q, want it to include %q", got, dir)
	}
}

func TestProbeBrowserReportsTheVersion(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "chrome")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho 'Chromium 132.0.6834.83'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	detail, err := probeBrowserBinary(context.Background(), binary)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.Contains(detail, "Chromium 132.0.6834.83") {
		t.Errorf("detail = %q, want the version the binary printed", detail)
	}
}

// The loader message arrives on stderr and exec only reports the exit status, so
// the hint has to see the combined output or it stays silent on exactly the
// failure it exists for.
func TestProbeBrowserNamesTheLoaderFix(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "chrome")
	script := "#!/bin/sh\necho 'error while loading shared libraries: libatk-1.0.so.0: cannot open shared object file' >&2\nexit 127\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	detail, err := probeBrowserBinary(context.Background(), binary)
	if err == nil {
		t.Fatalf("probe = %q, want an error", detail)
	}
	if !strings.Contains(err.Error(), "libatk-1.0.so.0") {
		t.Errorf("error = %q, want the missing library named", err)
	}
	if !strings.Contains(err.Error(), "CERAA_BROWSER_LD_LIBRARY_PATH") {
		t.Errorf("error = %q, want the variable that fixes it", err)
	}
}

func browserLaunchEnvValue() string {
	for _, entry := range browserLaunchEnv() {
		if strings.HasPrefix(entry, "LD_LIBRARY_PATH=") {
			return entry
		}
	}
	return ""
}

type errString string

func (e errString) Error() string { return string(e) }

// The loader takes the first LD_LIBRARY_PATH in the environment, so a second
// entry would be ignored. This is the bug the first version of this had, and it
// was invisible: the launch failed exactly as it did before the fix.
func TestBrowserLaunchEnvCarriesExactlyOneLibraryPath(t *testing.T) {
	dir := t.TempDir()
	// Set, not empty, because a host that exports it is the case that breaks.
	t.Setenv("LD_LIBRARY_PATH", "/some/other/place")

	real := browserSharedLibDirs
	t.Cleanup(func() { browserSharedLibDirs = real })
	browserSharedLibDirs = []string{dir}

	var found []string
	for _, entry := range browserLaunchEnv() {
		if strings.HasPrefix(entry, "LD_LIBRARY_PATH=") {
			found = append(found, entry)
		}
	}
	if len(found) != 1 {
		t.Fatalf("launch environment carries %d LD_LIBRARY_PATH entries, want 1: %q", len(found), found)
	}
	if !strings.Contains(found[0], dir) {
		t.Errorf("LD_LIBRARY_PATH = %q, want it to include %q", found[0], dir)
	}
	if strings.Contains(found[0], "/some/other/place") {
		t.Errorf("LD_LIBRARY_PATH = %q, want the ambient value folded in by browserLibraryPath", found[0])
	}
	browserSharedLibDirs = real
}
