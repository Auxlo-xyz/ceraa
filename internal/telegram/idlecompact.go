package telegram

import (
	"context"
	"fmt"
	"time"

	"ceraa/harness/compaction"
	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
	"ceraa/internal/config"
	"ceraa/internal/logger"
)

// idleCompactInterval is how often idle sessions are checked for folding.
//
// Five minutes against an hour TTL means a crossing session is caught at most
// a twelfth of a TTL past the seam: still warm, so the fold's read is still a
// cache read. Shorter would catch the seam more precisely and wake the daemon
// more often for sessions that have nothing to fold.
const idleCompactInterval = 5 * time.Minute

// watchIdleSessions folds long idle sessions before their prompt-cache entry
// lapses, so a returning turn reprocesses the summary instead of the whole
// conversation.
//
// It is the complement of the keepalive: cachewarm spends a one-token refresh
// to hold the entry while the owner is likely to return (up to an hour), and
// this spends one summarisation to shrink the session once they have not. The
// fold runs while the entry is still alive, which is what makes the fold
// itself cheap: the summariser replays the live conversation, and a live
// prefix is a cache read.
//
// Only sessions bound to a chat are visited: they are the conversations with
// an owner who can return. A session with a run in flight is skipped, because
// the runner holds that transcript in memory and a second writer saving under
// it would lose turns. Silent by design: it repairs cost, not correctness, and
// a message every few minutes would be worse than the spend it prevents.
func watchIdleSessions() {
	ticker := time.NewTicker(idleCompactInterval)
	defer ticker.Stop()
	for {
		select {
		case <-shutdown.Done():
			return
		case <-ticker.C:
		}
		sweepIdleSessions(time.Now())
	}
}

// sweepIdleSessions folds every bound session the idle decision claims. One
// pass, serially: folds are rare (the growth gate admits one per idle
// episode) and a serial pass can never fold two sessions against each other.
func sweepIdleSessions(now time.Time) {
	cfg, err := config.Load()
	if err != nil {
		logger.Warn("idle compaction: " + err.Error())
		return
	}
	store, err := configuredStore()
	if err != nil {
		logger.Warn("idle compaction: " + err.Error())
		return
	}
	providerID := cfg.EffectiveProviderID()
	provider := activeProviderConfig(cfg, providerID)
	modelConfig := llm.FromProvider(provider.URL, provider.APIKey, provider.Model)
	modelConfig.PromptCacheMode = provider.PromptCacheMode
	modelConfig.PromptCacheTTL = provider.PromptCacheTTL
	ttl := llm.PromptCacheTTLFor(modelConfig)
	for chatID, sessionID := range defaultState.activeSessionIDs() {
		if _, running := activeRuns.Load(chatID); running {
			continue
		}
		foldIdleSession(cfg, store, sessionID, now, ttl)
	}
}

// foldIdleSession folds one session if it is idle past the cache seam. A run
// in flight is the caller's to exclude: this takes the session id only, so a
// caller that cannot prove quiet must not call it.
func foldIdleSession(cfg *config.Config, store *sessionstore.Store, sessionID string, now time.Time, ttl time.Duration) {
	lastActive, err := store.LastModified(sessionID)
	if err != nil {
		return
	}
	sess, err := store.Load(sessionID)
	if err != nil {
		logger.Warn(fmt.Sprintf("idle compaction %s: %s", sessionID, err.Error()))
		return
	}
	compactor := newCompactor(cfg, cfg.EffectiveProviderID(), sessionID)
	decision := compaction.DecideIdleCompact(now, lastActive, ttl,
		compactor.EstimatedContext(sess), compactor.GrowthSinceCheckpoint(sess),
		compactor.Settings.Auto)
	if !decision.Fold {
		return
	}
	// In-band like the Mini App button: the summariser sees the conversation
	// as the run saw it, on the still-warm prefix. Without a project there is
	// no live request to build, and the cold summariser stands in: a
	// full-price fold now still beats a full-price rewrite on every later turn.
	if builder, err := webLiveRequest(cfg, store, sess); err != nil {
		logger.Warn(fmt.Sprintf("idle compaction %s: %s", sessionID, err.Error()))
		return
	} else if builder != nil {
		compactor.LiveRequest = builder
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	folded, err := compactor.Compact(ctx, sess)
	if err != nil {
		logger.Warn(fmt.Sprintf("idle compaction %s: %s", sessionID, err.Error()))
		return
	}
	if !folded {
		return
	}
	// Persisting is what makes this a compaction rather than a preview: the
	// summary is discarded when the request ends, and the next run re-reads
	// the full history just paid to fold. The write also moves the file's
	// mtime, which retires this idle episode so the next sweep leaves it alone
	// even before the growth gate would.
	if err := store.Save(sess); err != nil {
		logger.Warn(fmt.Sprintf("idle compaction %s: %s", sessionID, err.Error()))
		return
	}
	// A pending keepalive for this session now names a stale signature and
	// refuses itself on arrival, so there is nothing to disarm: the refresh
	// checks the signature before spending anything.
	logger.Info(fmt.Sprintf("idle compaction %s: folded after %s idle (%s)", sessionID, now.Sub(lastActive).Round(time.Minute), compactor.LastOutcome))
}
