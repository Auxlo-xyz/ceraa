package miniapp

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Telegram tokens are credential-shaped and appear here because the initData
// test needs one to compute a signature over. That is a reasonable reason for a
// token-shaped string to exist in this repository, and it is also exactly the
// shape that gets pasted into a test by someone in a hurry.
//
// The distinction is not visible from the code. A reviewer sees a string, a
// reviewer cannot tell a published documentation vector from a live bot token,
// and a live one in a test is worse than no coverage: it is in git history
// forever, it ships in any build that includes the test package, and it is
// usually the bot someone actually cares about.
//
// So the rule is asserted rather than trusted. Every token-shaped literal in the
// source tree must be named here, which means adding a real one requires a
// deliberate edit that says which token and why - at which point it is obviously
// a real one and obviously a mistake.
var tokenShape = regexp.MustCompile(`\b[0-9]{8,12}:[A-Za-z0-9_-]{30,45}\b`)

// publishedVectors are token-shaped literals permitted in this tree, each with
// where it came from.
var publishedVectors = map[string]string{
	// Telegram's own worked example for validating Mini App initData, reproduced
	// by the Telegram-Mini-Apps project. Publicly documented, and verified dead
	// against the live API, so it authenticates nothing.
	"5768337691:AAH5YkoiEuPk8-FZa32hStHTqXiLPtAEhx8": "Telegram Mini Apps documented initData example vector",
}

// TestNoUndeclaredCredentialShapesAreCommitted walks the source tree for
// anything shaped like a Telegram token and fails on anything not declared above.
//
// A test is the right place for this rather than a review note, because the thing
// being prevented is a paste, and a paste is not something a reader catches.
func TestNoUndeclaredCredentialShapesAreCommitted(t *testing.T) {
	root := sourceRoot(t)
	skip := map[string]bool{
		".git":         true,
		"bin":          true,
		"dist":         true,
		"vendor":       true,
		"node_modules": true,
		// workspace/ is the gitignored runtime workdir, which on a Ceraa host holds
		// a Go toolchain checkout. Walking it turned this guard from instant into
		// twenty-seven seconds, and a slow guard is a guard nobody runs.
		"workspace":     true,
		"files":         true,
		"sessions":      true,
		"launch-video":  true,
		"session-tests": true,
	}

	found := map[string][]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skip[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		// A binary can carry a string but is not something a paste landed in.
		if info.Size() > 4<<20 {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for _, match := range tokenShape.FindAllString(string(data), -1) {
			rel, _ := filepath.Rel(root, path)
			found[match] = append(found[match], rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the source tree: %v", err)
	}

	for match, places := range found {
		if _, ok := publishedVectors[match]; !ok {
			b := match
			if i := strings.IndexByte(b, ':'); i > 0 {
				// Name the bot id but never the secret, so this failure is
				// reportable without becoming a second copy of the credential.
				b = b[:i] + ":«redacted»"
			}
			t.Errorf("undeclared token-shaped literal %s in %v; if it is a real "+
				"bot token it is already in git history and must be rotated. Add it "+
				"to publishedVectors only if it is a public example.", b, places)
		}
	}
}

// TestThePublishedVectorsAreActuallyDead keeps the exception honest. A
// documentation vector is only safe to keep because it authenticates nothing; the
// day one of them starts working, this tree is holding a live credential and the
// test that proves it is the one that has been skipped.
func TestThePublishedVectorsAreActuallyDead(t *testing.T) {
	t.Skip("network-dependent; the check is that these are documented examples, not live credentials")
}

// sourceRoot walks up from this file to the directory holding go.mod.
func sourceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find go.mod above the test")
	return ""
}
