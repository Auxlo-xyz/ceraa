package tool

import (
	"context"
	"testing"
)

// A browser launched by a tool call is a child of the Ceraa process. Terminating
// Ceraa reparents Chromium to init rather than stopping it, so without an
// explicit teardown the browser tree outlives the daemon. This was observed as 23
// orphaned Chromium processes surviving 21 restarts.
func TestCloseLiveBrowsersClosesAnOpenDriver(t *testing.T) {
	m := &BrowserManager{newDriver: func(context.Context) (browserDriver, error) {
		return &fakeDriver{}, nil
	}}
	trackManager(m)
	t.Cleanup(func() { untrackManager(m) })

	if _, err := m.Driver(context.Background()); err != nil {
		t.Fatalf("open driver: %v", err)
	}
	if closed := CloseLiveBrowsers(); closed != 1 {
		t.Errorf("CloseLiveBrowsers = %d, want 1", closed)
	}
	if m.driver != nil {
		t.Error("driver was not released")
	}
}

func TestCloseLiveBrowsersIsSafeWithNoBrowserOpen(t *testing.T) {
	if closed := CloseLiveBrowsers(); closed != 0 {
		t.Errorf("CloseLiveBrowsers with no live browser = %d, want 0", closed)
	}
}

// A closed manager must not stay registered, or a long-lived daemon that builds a
// registry per run accumulates one entry per run for the life of the process.
func TestClosingAManagerUntracksIt(t *testing.T) {
	m := &BrowserManager{newDriver: func(context.Context) (browserDriver, error) {
		return &fakeDriver{}, nil
	}}
	trackManager(m)
	if _, err := m.Driver(context.Background()); err != nil {
		t.Fatalf("open driver: %v", err)
	}
	if _, err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	liveManagersMu.Lock()
	_, still := liveManagers[m]
	liveManagersMu.Unlock()
	if still {
		t.Error("a closed manager is still tracked")
	}
	// A second shutdown pass must not try to close it again.
	if closed := CloseLiveBrowsers(); closed != 0 {
		t.Errorf("CloseLiveBrowsers after an explicit close = %d, want 0", closed)
	}
}
