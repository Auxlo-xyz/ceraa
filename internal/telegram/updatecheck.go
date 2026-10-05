package telegram

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"ceraa/internal/logger"
	"ceraa/internal/selfupdate"
)

// updateCheckBudget bounds the startup advisory. Long enough to survive a slow
// link, short enough that a hung endpoint is abandoned while the bridge is
// still coming up rather than minutes later.
const updateCheckBudget = 12 * time.Second

// lastUpdateCheck keeps the startup check's answer for surfaces that report
// it. A status poll must never make a network call, so the dashboard reads
// this instead of probing: no answer yet means the check has not run, which
// is itself the state worth showing.
var lastUpdateCheck = struct {
	mu      sync.Mutex
	checked bool
	status  selfupdate.Status
}{}

func lastUpdateResult() (selfupdate.Status, bool) {
	lastUpdateCheck.mu.Lock()
	defer lastUpdateCheck.mu.Unlock()
	return lastUpdateCheck.status, lastUpdateCheck.checked
}

// updateCheckActive is what the dashboard shows: machine policy first, then
// the persisted preference. Either one off means off.
func updateCheckActive() bool {
	return os.Getenv("CERAA_DISABLE_UPDATE_CHECK") == "" && defaultState.updateCheckEnabled()
}

// noteNewRelease logs when a newer release exists.
//
// Backgrounded and bounded, because the alternative is a startup that waits on
// somebody else's server: a release endpoint that is slow, down, or firewalled
// must not delay or fail the bridge, and the person waiting for the bot does not
// care whether a new version exists.
//
// It logs the failure too. In a log that says nothing, "could not reach" and
// "you are current" are indistinguishable, and only one of them is actionable.
// This is the whole reason a shipped release is otherwise invisible: nothing
// looks, so nothing can report.
//
// CERAA_DISABLE_UPDATE_CHECK turns it off for a machine that must not make the
// outbound call at all.
func noteNewRelease() {
	if os.Getenv("CERAA_DISABLE_UPDATE_CHECK") != "" || !defaultState.updateCheckEnabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateCheckBudget)
	defer cancel()
	status, err := selfupdate.Check(ctx)
	if err != nil {
		logger.Warn("Update check failed: " + err.Error())
		return
	}
	lastUpdateCheck.mu.Lock()
	lastUpdateCheck.status = status
	lastUpdateCheck.checked = true
	lastUpdateCheck.mu.Unlock()
	if status.Available {
		logger.Info(fmt.Sprintf("Update available: %s (running %s); run `ceraa update`", status.Latest, status.Current))
	}
}
