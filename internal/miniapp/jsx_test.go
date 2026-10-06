package miniapp

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The dashboard is JSX in an HTML file, compiled in the browser by Babel. Every
// Go test in this package reads that file as text, so none of them can tell a
// syntax error from a valid file: a duplicate const declaration, a stray brace
// from a bad splice, a component used before its declaration. Those all ship.
//
// esbuild parses JSX, and one is vendored on this host. When it is not present
// the test reports that it skipped rather than failing, because a release
// machine without esbuild is not a broken build.
// The regex cannot express "not a src script" the way Go's engine can, so it
// matches every block and the src ones are dropped by hand below. Go's RE2 has
// no lookahead, which is why this is not written the obvious way.
var anyScriptBlock = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)

func TestTheDashboardCompiles(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("assets", "index.html"))
	if err != nil {
		t.Fatalf("read the dashboard: %s", err)
	}
	matches := anyScriptBlock.FindAllSubmatch(page, -1)
	if len(matches) == 0 {
		t.Fatal("no script block found in the dashboard")
	}
	longest := ""
	for _, m := range matches {
		// The tag attributes are not captured, so a block whose body is empty
		// is a src reference. The app script is by far the longest body, and
		// requiring a real length keeps a stray empty block from winning.
		if len(m[1]) > len(longest) {
			longest = string(m[1])
		}
	}
	if strings.TrimSpace(longest) == "" {
		t.Fatal("the dashboard has no inline script to check")
	}
	esbuild := findEsbuild()
	if esbuild == "" {
		t.Skip("esbuild not on PATH; skipping the dashboard compile check")
	}
	tmp := filepath.Join(t.TempDir(), "dashboard.js")
	if err := os.WriteFile(tmp, []byte(longest), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(esbuild, "--loader:.js=jsx", "--outfile="+filepath.Join(t.TempDir(), "out.js"), tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the dashboard does not compile: %s\n%s", err, out)
	}
}

// findEsbuild looks for an esbuild binary near this package, then on PATH.
//
// It deliberately searches upwards rather than only checking PATH: the one
// esbuild on a Ceraa host belongs to an unrelated vendored project, so a
// PATH-only lookup skips the check in exactly the environment where the
// dashboard is being edited. A skip is still possible on a bare machine, which
// is why the skip says so rather than passing quietly.
func findEsbuild() string {
	var candidates []string
	if root, err := os.Getwd(); err == nil {
		for i := 0; i < 6; i++ {
			candidates = append(candidates,
				filepath.Join(root, "node_modules", ".bin", "esbuild"),
				filepath.Join(root, "node_modules", "esbuild", "bin", "esbuild"),
				filepath.Join(root, "node_modules", "@esbuild", "linux-x64", "bin", "esbuild"),
			)
			root = filepath.Dir(root)
		}
	}
	if p, err := exec.LookPath("esbuild"); err == nil {
		candidates = append(candidates, p)
	}
	// Sibling trees. An esbuild installed for another project on the same host
	// is still an esbuild, and searching only this tree's ancestors finds
	// nothing on a machine that has one - which is how this check skipped
	// silently on the box that needed it.
	for _, sibling := range []string{
		"/workspace/hermes-agent/node_modules/.bin/esbuild",
		"/workspace/hermes-agent/node_modules/@esbuild/linux-x64/bin/esbuild",
	} {
		candidates = append(candidates, sibling)
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}
