package whatsapp

import (
	"testing"

	"ceraa/internal/testsupport"
)

// TestMain redirects this test binary away from the live install, matching
// every other package that logs. It also isolates the data directory, which is
// where the hub-editable allow-list defaults to, so a test that adds an entry
// cannot touch the running daemon's list.
func TestMain(m *testing.M) {
	testsupport.Main(m)
}
