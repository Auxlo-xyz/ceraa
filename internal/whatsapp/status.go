package whatsapp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"ceraa/internal/paths"
)

// Status is what the hub renders without a live client. The daemon writes
// it whenever linking changes; the menus read it, because the menus
// package cannot reach the daemon's transport and must not open the
// session database a second time while the daemon holds it open.
//
// Enabled is deliberately absent: the environment already carries it, and
// a copy in this file would be a second answer to the same question that
// could drift. The menus read Enabled from LoadConfig and the rest here.
type Status struct {
	Linked bool   `json:"linked"`
	JID    string `json:"jid,omitempty"`
}

// statusPath is indirected so a test can point it at a temp file without
// setting CERAA_HOME, which paths.DataDir caches and would leak between
// tests. Mirrors the runtimeStatePath pattern in the menus package.
var statusPath = ""

// StatusPath is the file holding the last known link state.
func StatusPath() string {
	if statusPath != "" {
		return statusPath
	}
	return filepath.Join(paths.DataDir(), "whatsapp-status.json")
}

// LoadStatus reads the last written link state. A missing or unreadable
// file reads as not linked: the safe default is "nothing is paired",
// which shows a Link button rather than a false "linked".
func LoadStatus() Status {
	data, err := os.ReadFile(StatusPath())
	if err != nil {
		return Status{}
	}
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return Status{}
	}
	s.JID = strings.TrimSpace(s.JID)
	return s
}

// SaveStatus writes the link state atomically. A write failure is not
// fatal to the daemon; it only costs a stale menu label until the next
// successful write, so the error is returned rather than panicked.
func SaveStatus(s Status) error {
	path := StatusPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".whatsapp-status-*")
	if err != nil {
		return err
	}
	enc, err := json.Marshal(s)
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if _, err := tmp.Write(enc); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// SetStatusPathForTest pins the status file for a test and returns a
// restore function. Kept in the shipped package rather than a _test file
// because the menus tests, in another package, need it.
func SetStatusPathForTest(path string) func() {
	previous := statusPath
	statusPath = path
	return func() { statusPath = previous }
}
