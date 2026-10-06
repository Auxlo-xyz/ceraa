package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/internal/logger"
	"ceraa/internal/paths"
	"ceraa/internal/testsupport"
)

// TestMain keeps this package's logs out of the live install.
//
// This package builds a coordinator, and the coordinator logs "Context budget
// recalibrated" as it starts a run. So the log lines this suite produced came
// from harness/coordinator while the leak was attributed to that package. The
// guard has to be where the code is *driven*, not only where it is written:
// coordinator has its own TestMain and still could not stop this.
func TestMain(m *testing.M) {
	testsupport.Main(m)
}

// Guards the guard.
func TestTheLiveLogIsNotReachable(t *testing.T) {
	want := os.Getenv("CERAA_LOG_FILE")
	if want == "" {
		t.Fatal("CERAA_LOG_FILE is unset, so the logger resolves into the live log")
	}
	if got := logger.Get().Path(); !strings.HasPrefix(got, filepath.Dir(want)) {
		t.Errorf("the logger writes to %q, outside the test directory holding %q", got, want)
	}
	if got := paths.DataDir(); got != os.Getenv("CERAA_HOME") {
		t.Errorf("the data dir %q is not the test directory %q", got, os.Getenv("CERAA_HOME"))
	}
}
