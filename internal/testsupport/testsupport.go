// Package testsupport redirects a test binary away from the live install.
//
// It exists because the redirect was written per package and only two packages
// had it. Go runs every package's tests in a separate binary, so an environment
// variable set by one package's TestMain is invisible to its siblings. The
// logger resolves its path lazily on first write and falls back to
// <data dir>/logs/bridge.log, so an unguarded package appends to the log the
// running bot is writing to - which is how a suite ended up interleaving
// `model "stub/model"` into a production log record.
//
// Every package that logs must call Main from its TestMain. Doing the work here
// rather than in each TestMain is the point: one implementation, and a package
// that forgets cannot silently fall back to the real directory.
//
// Main also strips the CERAA_* variables a parent shell may have exported. The
// redirect alone is not enough: the running supervisor puts its own CERAA_HOME
// and CERAA_SESSION_FILE into any shell that shares its
// environment, and those override the fixture a test just wrote. That is how
// `go test` started from the supervisor failed tests that pass in a clean one.
package testsupport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Main points this test binary at a temporary data directory and log file, runs
// the tests, and exits with their status.
//
// Call it as the whole body of a TestMain:
//
//	func TestMain(m *testing.M) { testsupport.Main(m) }
func Main(m *testing.M) {
	ScrubCeraaEnv()
	dir, err := os.MkdirTemp("", "ceraa-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("CERAA_HOME", dir); err != nil {
		panic(err)
	}
	// Set before any test can log: the logger is lazily initialised on its first
	// write, so this only has to precede the tests, not package initialisation.
	if err := os.Setenv("CERAA_LOG_FILE", filepath.Join(dir, "bridge.log")); err != nil {
		panic(err)
	}
	code := m.Run()
	// Removed on the way out so a crashed suite does not leave a directory
	// behind for every run to accumulate.
	os.RemoveAll(dir)
	os.Exit(code)
}

// ScrubCeraaEnv removes every inherited CERAA_* variable.
//
// A test binary configures itself: each test calls t.Setenv for the variable it
// depends on, and Main sets the data directory and log that follow. The values
// inherited from a parent process are therefore never wanted, and leaving them
// in place is not a harmless difference in environment, it changes what the
// test asserts. CERAA_SESSION_FILE moved the store out from under the manual
// compaction tests.
func ScrubCeraaEnv() {
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "CERAA_") {
			os.Unsetenv(name)
		}
	}
}
