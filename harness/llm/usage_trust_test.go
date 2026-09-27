package llm

import "testing"

// The endpoints observed returning a running total miss by two orders of
// magnitude. This is the figure that made cost and cache ratio fiction, so the
// band has to catch it while tolerating ordinary tokenizer disagreement.
func TestReconcileUsageRejectsARunningTotal(t *testing.T) {
	// We sent 112,375 tokens; the endpoint claimed 47,513,540.
	usage := UsageInfo{InputTokens: 47_513_540, CachedInputTokens: 100_532_640}
	local := TokenBreakdown{System: 517, Tools: 1197, User: 655, Conversation: 26010, ToolOutput: 83985, Reasoning: 11}

	trust := ReconcileUsage(usage, local)
	if trust.Trusted {
		t.Fatal("a 423x discrepancy was trusted")
	}
	if trust.Note == "" {
		t.Error("an untrusted verdict must say why, or it cannot be acted on")
	}
	if trust.LocalInput != local.Total() || trust.Reported != usage.InputTokens {
		t.Errorf("the discrepancy is not preserved: local=%d reported=%d", trust.LocalInput, trust.Reported)
	}
}

// Tokenizers differ and providers round, so a few percent must stay trusted. If
// this band were tight enough to reject ordinary disagreement, every real run
// would be marked unknown and the check would be worthless.
func TestReconcileUsageToleratesTokenizerDisagreement(t *testing.T) {
	for _, ratio := range []float64{1.0, 0.97, 1.05, 0.88, 1.01, 0.5, 2.0} {
		local := TokenBreakdown{ToolOutput: 100_000}
		usage := UsageInfo{InputTokens: int(float64(local.Total()) * ratio)}
		if trust := ReconcileUsage(usage, local); !trust.Trusted {
			t.Errorf("ratio %.2f was rejected; tokenizer disagreement is not a bad provider", ratio)
		}
	}
}

// A run we could not measure is not evidence of a bad provider, so it stays
// trusted. Otherwise a missing breakdown would silently suppress every cost.
func TestReconcileUsageTrustsAnUnmeasuredRun(t *testing.T) {
	if trust := ReconcileUsage(UsageInfo{InputTokens: 5000}, TokenBreakdown{}); !trust.Trusted {
		t.Error("an unmeasured run was marked untrusted")
	}
	if trust := ReconcileUsage(UsageInfo{}, TokenBreakdown{ToolOutput: 100}); !trust.Trusted {
		t.Error("a run with no reported usage was marked untrusted")
	}
}

// The point of the whole exercise: an untrusted figure must not yield a ratio.
// Cached and input are both possibly-cumulative, so their quotient drifts toward
// 1 by itself and reports excellent caching for a request that was never cached.
func TestCacheReadRatioForWithholdsUntrustedFigures(t *testing.T) {
	usage := UsageInfo{InputTokens: 100, CachedInputTokens: 99}
	if got := usage.CacheReadRatioFor(UsageTrust{Trusted: false}); got != 0 {
		t.Errorf("CacheReadRatioFor on untrusted usage = %v, want 0", got)
	}
	if got := usage.CacheReadRatioFor(UsageTrust{Trusted: true}); got < 0.98 {
		t.Errorf("CacheReadRatioFor on trusted usage = %v, want ~0.99", got)
	}
}
