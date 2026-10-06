package coordinator

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
// The logger resolves through $HOME, which the test process shares with the
// running bot, so a suite that only redirects CERAA_HOME still appends to the
// real bridge.log. That file is the record of what a provider actually did;
// "Context budget recalibrated: ... model \"test\"" interleaved into it makes it
// useless for the next diagnosis.
func TestMain(m *testing.M) {
	testsupport.Main(m)
}

// Guards the guard: if the logger resolves outside the temporary directory, a
// test run is writing to the user's real log again.
func TestTheLiveLogIsNotReachable(t *testing.T) {
	want := os.Getenv("CERAA_LOG_FILE")
	if want == "" {
		t.Fatal("CERAA_LOG_FILE is unset, so the logger resolves through $HOME into the live log")
	}
	if got := logger.Get().Path(); !strings.HasPrefix(got, filepath.Dir(want)) {
		t.Errorf("the logger writes to %q, outside the test directory containing %q", got, want)
	}
	if got := paths.DataDir(); got != os.Getenv("CERAA_HOME") {
		t.Errorf("the data dir %q is not the test directory %q", got, os.Getenv("CERAA_HOME"))
	}
}
