package recon

// main_test.go keeps the recon suite off the operator's data directory.
//
// The scope record is a file in the live data dir, and the tests write it.
// Writing test state into a live install's authorization file is not a
// test, it is a race with the running service, so the whole package points
// CERAA_HOME at a temporary directory for the duration of the run.

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ceraa-recon-suite")
	if err != nil {
		panic("recon suite needs a temporary data dir: " + err.Error())
	}
	os.Setenv("CERAA_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
