package whatsapp

import (
	"os"
	"path/filepath"
	"testing"
)

// A missing or corrupt status file reads as not linked, the safe default:
// the hub shows a Link button rather than claiming a link that is not there.
func TestStatusReadsSafeOnMissingAndCorrupt(t *testing.T) {
	restore := SetStatusPathForTest(filepath.Join(t.TempDir(), "whatsapp-status.json"))
	defer restore()

	if s := LoadStatus(); s.Linked || s.JID != "" {
		t.Fatalf("missing file = %+v, want zero status", s)
	}
	if err := os.WriteFile(StatusPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if s := LoadStatus(); s.Linked || s.JID != "" {
		t.Fatalf("corrupt file = %+v, want zero status", s)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	restore := SetStatusPathForTest(filepath.Join(t.TempDir(), "whatsapp-status.json"))
	defer restore()

	if err := SaveStatus(Status{Linked: true, JID: "2348012345678@s.whatsapp.net"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := LoadStatus()
	if !got.Linked || got.JID != "2348012345678@s.whatsapp.net" {
		t.Fatalf("round trip = %+v", got)
	}
	if err := SaveStatus(Status{}); err != nil {
		t.Fatalf("save empty: %v", err)
	}
	if got := LoadStatus(); got.Linked || got.JID != "" {
		t.Fatalf("after clear = %+v", got)
	}
}
