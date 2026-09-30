package build

import (
	"regexp"
	"strings"
	"testing"
)

// A release tag and the version the binary reports must agree. This asserts the
// shape rather than an exact string, because a plain `go build` legitimately
// reports a development version.
func TestVersionIsNotAStaleConstant(t *testing.T) {
	if Version == "" {
		t.Fatal("Version is empty")
	}
	// A hardcoded release version is the failure this guards: it stays 0.1.0
	// forever while the tags move on. A development default must say so.
	if !strings.Contains(Version, "-dev") && !semverish(Version) {
		t.Errorf("Version %q is neither a development marker nor a version number", Version)
	}
	if Version == "0.1.0" {
		t.Error("Version is the bare old constant; a build should be marked -dev or injected from the tag")
	}
}

var versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+`)

func semverish(v string) bool { return versionRe.MatchString(v) }

// The variable must be settable by the linker, which is the whole point of it
// being a var: -X cannot write a const.
func TestVersionIsLinkerSettable(t *testing.T) {
	// A const cannot be assigned; this fails to compile if Version becomes one,
	// which is the regression this file exists to prevent.
	old := Version
	Version = "9.9.9-probe"
	if Version != "9.9.9-probe" {
		t.Fatal("Version is not assignable")
	}
	Version = old
}
