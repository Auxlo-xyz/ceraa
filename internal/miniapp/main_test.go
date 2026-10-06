package miniapp

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
// The Mini App server warns on every refused request and notes every provisioned
// asset, so its tests produced real lines in the running bot's bridge.log.
func TestMain(m *testing.M) {
	testsupport.Main(m)
}

// Guards the guard: the Mini App tests bind sockets and read assets, and the
// logger is lazily initialised on whichever test writes first, so the
// redirection has to be proven rather than assumed.
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
