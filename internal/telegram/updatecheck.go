package telegram

import (
	"context"
	"fmt"
	"os"
	"time"

	"ceraa/internal/logger"
	"ceraa/internal/selfupdate"
)

// updateCheckBudget bounds the startup advisory. Long enough to survive a slow
// link, short enough that a hung endpoint is abandoned while the bridge is
// still coming up rather than minutes later.
const updateCheckBudget = 12 * time.Second

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
	if os.Getenv("CERAA_DISABLE_UPDATE_CHECK") != "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateCheckBudget)
	defer cancel()
	status, err := selfupdate.Check(ctx)
	if err != nil {
		logger.Warn("Update check failed: " + err.Error())
		return
	}
	if status.Available {
		logger.Info(fmt.Sprintf("Update available: %s (running %s); run `ceraa update`", status.Latest, status.Current))
	}
}
