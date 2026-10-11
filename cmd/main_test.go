package cmd

import (
	"testing"

	"ceraa/internal/testsupport"
)

// TestMain strips the CERAA_* variables a parent shell may have exported before
// any command test runs.
//
// runSetup resolves the config path through the same environment readers as the
// daemon, so an inherited CERAA_HOME moved the file the setup test then tried to
// read: it failed with "open /.config/ceraa/config.json: no such file or
// directory" in a shell shared with the supervisor and passed in a clean one.
func TestMain(m *testing.M) {
	testsupport.Main(m)
}
