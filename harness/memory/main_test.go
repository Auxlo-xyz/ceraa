package memory

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
// The memory package logs from extract.go on every commit, so an unguarded
// suite writes those lines into the running bot's bridge.log. It was the source
// of `model "stub/model"` appearing in a production log record.
func TestMain(m *testing.M) {
	testsupport.Main(m)
}

// Guards the guard: without this the redirection is an assumption, and an
// assumption about a production log is not one worth making.
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
