package telegram

import (
	"os"
	"testing"

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
