package llm

import (
	"ceraa/internal/logger"
	"fmt"
)

// Per-request cache accounting for the LLM layer.
//
// RecordTurn writes one log line per model request carrying the
// provider-reported input tokens, cached input tokens and output tokens,
// the computed hit rate, and our own local estimate. It is log-only: it
// never returns an error and never fails the calling request.

// cacheHitPercent converts the provider-reported usage into a 0-100 hit
// percentage using the same split-reporting convention as
// UsageInfo.CacheReadRatio.
func cacheHitPercent(reported UsageInfo) float64 {
	return reported.CacheReadRatio() * 100
}

// RecordTurn logs one line of per-request cache accounting.
//
// It is a no-op when the provider reported zero input tokens, because
// there is nothing to attribute a hit rate to. It never panics and never
// reports an error; logging must not fail a model request.
func RecordTurn(providerHost, model string, reported UsageInfo, localEstimate int) {
	defer func() {
		_ = recover()
	}()
	if reported.InputTokens <= 0 {
		return
	}
	hit := cacheHitPercent(reported)
	func() {
		defer func() {
			_ = recover()
		}()
		logger.Info(fmt.Sprintf(
			"cacheacct host=%s model=%s input=%d cached=%d output=%d hit=%.1f%% local_estimate=%d",
			providerHost, model, reported.InputTokens, reported.CachedInputTokens,
			reported.OutputTokens, hit, localEstimate))
	}()
}
