package telegram

import (
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"ceraa/internal/telegram/menus"
)

// /restart must appear in the menu and help catalog, because that catalog is
// what setMyCommands and /help render from. A handler without a catalog entry
// is a command nobody can discover.
func TestRestartCommandListed(t *testing.T) {
	for _, spec := range menus.Commands() {
		if spec.Command == "restart" {
			return
		}
	}
	t.Error("restart missing from the command catalog")
}

// The arm/report path is the whole restart decision as far as cmd/root.go is
// concerned, so it is pinned without touching the network or signals.
func TestRestartArmsAndReports(t *testing.T) {
	t.Cleanup(func() {
		restartState.Lock()
		restartState.armed = false
		restartState.chatID = 0
		restartState.Unlock()
		os.Unsetenv(restartNoticeEnv)
	})
	if RestartRequested() {
		t.Fatal("restart reported as requested before anything armed it")
	}
	armRestart(12345)
	if !RestartRequested() {
		t.Error("restart not reported after arming")
	}
	restartState.Lock()
	chatID := restartState.chatID
	restartState.Unlock()
	if chatID != 12345 {
		t.Errorf("restart chat = %d, want 12345", chatID)
	}
}

// The back-online notice crosses the exec through the environment, and the
// boot side must consume it exactly once: a notice that survives would greet
// the chat again on every later boot.
func TestRestartNoticeRoundTrip(t *testing.T) {
	t.Cleanup(func() { os.Unsetenv(restartNoticeEnv) })
	armRestart(67890)
	chatID, ok := takeRestartNotice()
	if !ok || chatID != 67890 {
		t.Fatalf("notice = (%d, %v), want (67890, true)", chatID, ok)
	}
	if _, ok := takeRestartNotice(); ok {
		t.Error("notice survived a take; it would repeat on the next boot")
	}
}

func TestRestartNoticeRejectsGarbage(t *testing.T) {
	t.Cleanup(func() { os.Unsetenv(restartNoticeEnv) })
	for _, raw := range []string{"", "not-a-number", "0"} {
		os.Setenv(restartNoticeEnv, raw)
		if _, ok := takeRestartNotice(); ok {
			t.Errorf("notice %q accepted, want rejection", raw)
		}
	}
}

// The CLI restart signals the process rather than stopping and starting it,
// so the signal must funnel into the same armed flag cmd/root.go execs on.
// The shutdown itself is stubbed: cancelling the shared context here would
// leak into every other test in the package.
func TestRestartSignalArmsRestart(t *testing.T) {
	t.Cleanup(func() {
		restartState.Lock()
		restartState.armed = false
		restartState.chatID = 0
		restartState.Unlock()
		os.Unsetenv(restartNoticeEnv)
		restartShutdown = func() { stopSignals() }
	})
	var stopped atomic.Bool
	restartShutdown = func() { stopped.Store(true) }
	WatchRestartSignal()
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatalf("signal self: %s", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for (!RestartRequested() || !stopped.Load()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !RestartRequested() {
		t.Fatal("SIGUSR1 did not arm the restart")
	}
	if !stopped.Load() {
		t.Error("SIGUSR1 armed the restart but did not stop the process")
	}
}
