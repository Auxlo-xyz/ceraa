package prompt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	sourceprompt "ceraa/prompt"
)

func TestEmbeddedPromptIsNonEmpty(t *testing.T) {
	if strings.TrimSpace(string(sourceprompt.System)) == "" {
		t.Fatal("embedded prompt is empty")
	}
}

func TestReleasedBinaryRunsWithoutPromptSidecar(t *testing.T) {
	if os.Getenv("CERAA_PROMPT_TEST_CHILD") == "1" {
		load()
		return
	}
	bin, err := filepath.Abs(filepath.Join("..", "..", "dist", "ceraa-linux-amd64"))
	if err != nil {
		t.Skipf("cannot resolve release binary: %v", err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("no built release binary; run scripts/release.sh first")
	}
	work := t.TempDir()
	cmd := exec.Command(bin, "version")
	cmd.Dir = work
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the released binary: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "minimal fallback") {
		t.Errorf("the binary fell back without a prompt sidecar:\n%s", out)
	}
}

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
		t.Errorf("the binary reports %q but the release is tagged %q", got, release)
	}
}
