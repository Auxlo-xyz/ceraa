package prompt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A packaged install ships the binary and prompt/system.txt side by side. This
// asserts the two agree, because the failure it guards is silent: the binary
// starts fine and simply runs on a one-line fallback, so nobody notices until the
// agent behaves unlike the development build.
func TestPackagedInstallIsNotDegraded(t *testing.T) {
	dist, err := filepath.Abs(filepath.Join("..", "..", "dist"))
	if err != nil {
		t.Skipf("cannot resolve dist: %v", err)
	}
	assets, err := filepath.Glob(filepath.Join(dist, "ceraa-linux-*"))
	if err != nil || len(assets) == 0 {
		t.Skip("no linux binaries in dist; run scripts/build.sh then the release build first")
	}
	// The prompt that ships must be the prompt in the tree, byte for byte.
	shipped, err := os.ReadFile(filepath.Join(dist, "system.txt"))
	if err != nil {
		t.Fatalf("dist/system.txt is missing, so a packaged install runs degraded: %v", err)
	}
	source, err := os.ReadFile(filepath.Join("..", "..", "prompt", "system.txt"))
	if err != nil {
		t.Fatalf("prompt/system.txt: %v", err)
	}
	if string(shipped) != string(source) {
		t.Error("dist/system.txt differs from prompt/system.txt; the released prompt is stale")
	}
}

// The loader must find the prompt beside the executable, because that is the only
// location a packaged install can put it.
func TestLoaderFindsThePromptBesideTheExecutable(t *testing.T) {
	// candidates is the resolution order, and it must include the executable's own
	// directory or a packaged install can never load a prompt.
	paths := candidates()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test executable: %v", err)
	}
	dir := filepath.Dir(exe)
	want := filepath.Join(dir, SystemPromptPath)
	for _, p := range paths {
		if p == want {
			return
		}
	}
	t.Errorf("candidates %q do not include %q", paths, want)
}

// A missing prompt must be loud, because "starts fine but behaves wrong" is the
// worst way for it to fail.
func TestMissingPromptIsReported(t *testing.T) {
	if os.Getenv("CERAA_PROMPT_TEST_CHILD") == "1" {
		// In the child, load is expected to complain. The harness below checks it did.
		load()
		return
	}
	// Run a child with an empty working directory and no override, so no candidate
	// can resolve, and assert the process printed the warning.
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestMissingPromptIsReported")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"CERAA_PROMPT_TEST_CHILD=1",
		"CERAA_SYSTEM_PROMPT=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The test binary may exit non-zero from an unrelated failure; the message
		// is what matters and it is printed during package init.
		_ = err
	}
	if !strings.Contains(string(out), "system prompt not found") {
		t.Errorf("a missing prompt was not reported:\n%s", out)
	}
}

// A released binary must report the version its release is tagged with. The
// version was a compile-time constant once, so v0.1.1 shipped a binary that
// reported 0.1.0 and nothing failed. This runs the built binary when one is
// present and compares it to the version asked for.
func TestReleasedBinaryReportsTheReleaseVersion(t *testing.T) {
	release := os.Getenv("CERAA_RELEASE_VERSION")
	if release == "" {
		t.Skip("set CERAA_RELEASE_VERSION to the tag being verified")
	}
	dist, err := filepath.Abs(filepath.Join("..", "..", "dist"))
	if err != nil {
		t.Skipf("cannot resolve dist: %v", err)
	}
	bin := filepath.Join(dist, "ceraa-linux-amd64")
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("no built linux binary: %v", err)
	}
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("running the release binary: %v\n%s", err, out)
	}
	got := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	want := "ceraa " + strings.TrimPrefix(release, "v")
	if got != want {
		t.Errorf("the binary reports %q but the release is tagged %q", got, want)
	}
}
