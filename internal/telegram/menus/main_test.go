package menus

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
// menus.go logs the provider label on every hub build, which is how duplicate
// "hub label minimax" lines ended up in the running bot's bridge.log.
func TestMain(m *testing.M) {
	testsupport.Main(m)
}

// Guards the guard: this package writes log lines as a side effect of building
// a menu, which is most of what it is for, so the redirection is load-bearing.
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
