package telegram

import (
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// restartNoticeEnv carries the chat that asked for the restart across the exec,
// so the new process can report that it is back. The environment survives
// syscall.Exec; package state does not.
const restartNoticeEnv = "CERAA_RESTART_NOTICE_CHAT"

// /restart shuts the bot down cleanly and starts it again as the same binary
// with the same arguments and environment.
//
// It is an exec rather than an exit because there is no supervisor. The daemon
// slot is a pid file, not a process manager, so a plain exit would leave the
// bot down until an operator came back. syscall.Exec replaces the process
// image in place: the pid file defer in Run removes the old claim, the new
// image re-claims it, and the pid does not even change.
//
// Owner-only by construction. The poll loop rejects any sender who is not the
// allowed user before any command runs, so no second check lives here. A
// restart from a stranger would be a remote shutdown button, which is why the
// gate order matters more than the handler.

var restartState struct {
	sync.Mutex
	armed  bool
	chatID int64
}

// handleRestart tells the chat what is happening, arms the restart, and then
// stops the process through the same signal path a SIGTERM takes. Going
// through stopSignals matters: the poll loop, the watchers and the deferred
// cleanup in Run all key off the shutdown context, so every close hook runs
// before cmd/root.go execs the new image. The stop runs on a short delay so
// the confirmation send above has left the building first.
func handleRestart(token string, chatID int64) {
	send(token, chatID, "Restarting Ceraa. Back in a moment.")
	armRestart(chatID)
	go func() {
		time.Sleep(2 * time.Second)
		stopSignals()
	}()
}

// armRestart records a restart request. Split from handleRestart so tests can
// drive the flag without touching the network or the signal context.
func armRestart(chatID int64) {
	restartState.Lock()
	defer restartState.Unlock()
	restartState.armed = true
	restartState.chatID = chatID
	// Set here rather than at exec time so nothing between Run returning and
	// the exec can lose it. The boot side consumes and unsets it.
	os.Setenv(restartNoticeEnv, strconv.FormatInt(chatID, 10))
}

// RestartRequested reports whether the last Run ended by restart. cmd/root.go
// checks it after Run returns and execs the new image when it is set.
func RestartRequested() bool {
	restartState.Lock()
	defer restartState.Unlock()
	return restartState.armed
}

// takeRestartNotice consumes the pending back-online notice, if any. Called
// once, at boot, after the bot token is known.
func takeRestartNotice() (int64, bool) {
	raw := os.Getenv(restartNoticeEnv)
	if raw == "" {
		return 0, false
	}
	os.Unsetenv(restartNoticeEnv)
	chatID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || chatID == 0 {
		return 0, false
	}
	return chatID, true
}

// sendRestartNotice reports back-online to the chat that asked for the
// restart. Best effort: a restart that cannot reach Telegram is still a
// restart, and must not fail the boot.
func sendRestartNotice(token string) {
	chatID, ok := takeRestartNotice()
	if !ok {
		return
	}
	// Recorded before the tunnel is published, which happens a few seconds
	// later on the boot path. Without this a restart has no chat to push a
	// fresh address to, because the address is minted after this notice and
	// nothing else has recorded one yet.
	noteMiniAppChat(chatID)
	send(token, chatID, "Back online. Restart complete.")
}

// restartShutdown stops the process the way a SIGTERM would. A variable
// rather than a direct call so tests can observe the signal path without
// cancelling the shared shutdown context the whole package reads.
var restartShutdown = func() { stopSignals() }

var restartWatchOnce sync.Once

// WatchRestartSignal arms the same self-exec restart when the process
// receives SIGUSR1. This is the CLI half: `ceraa restart` signals the running
// daemon instead of stopping and starting it, so both surfaces funnel into
// one shutdown path and one exec. SIGUSR1 rather than SIGHUP because a
// hangup must never reboot the bot; USR1 carries no such baggage.
func WatchRestartSignal() {
	restartWatchOnce.Do(func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGUSR1)
		go func() {
			for range sig {
				// Chat zero means no Telegram notice: the CLI reports the
				// outcome itself, and takeRestartNotice rejects zero anyway.
				armRestart(0)
				restartShutdown()
			}
		}()
	})
}

// ExecSelf replaces the running process with a fresh instance of the same
// binary, arguments and environment. It returns only when the exec itself
// fails, in which case the caller reports the error and the process exits
// without restarting.
func ExecSelf() error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(bin, os.Args, os.Environ())
}
