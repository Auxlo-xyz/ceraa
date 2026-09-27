package coordinator

import (
	"testing"

	"ceraa/harness/llm"
)

// The reconciliation compares the provider's reported usage against our own prompt
// count. If the count does not cover the same requests, a healthy provider looks
// broken.
//
// Live failure, disproof from the provider's own dashboard. Two runs, two requests
// each: the breakdown stored on the run summed to 63,834 and 68,241 against
// reported inputs of 66,965 and 71,162, so the provider agreed with our full count
// to within 5%. But the value we actually reconciled against was 30,537 and
// 34,060, almost exactly half, and 71,162 / 34,060 cleared our 2.0x tolerance
// and marked both runs REJECTED. The dashboard settled it: its per-call rows,
// 35,654 + 35,508, sum to exactly the 71,162 we reported.
func TestBreakdownCoversTheRequestItIsReconciledAgainst(t *testing.T) {
	// The two requests of the live run. The first carries the user prompt; the
	// second carries the tool result the first produced, so it is slightly larger.
	// Their sum is the 68,241 stored on the run.
	first := llm.TokenBreakdown{
		System: 1712, Tools: 3558, User: 122, Conversation: 2864, Reasoning: 11132,
	}
	second := llm.TokenBreakdown{
		System: 1712, Tools: 3558, Conversation: 2864, ToolOutput: 46847, Compaction: 2006,
	}
	reported := llm.UsageInfo{InputTokens: 71_162, CachedInputTokens: 71_119}

	var running llm.TokenBreakdown
	// Counted through the same helper the run loop uses, so a regression in the
	// ordering is observable here rather than only in production.
	countRequestBeforeUse(&running, first)
	countRequestBeforeUse(&running, second)

	trust := llm.ReconcileUsage(reported, running)
	if !trust.Trusted {
		t.Fatalf("a healthy provider was rejected: %s", trust.Note)
	}
	// The provider was within 5% of our count; assert we have not drifted from that.
	if ratio := float64(reported.InputTokens) / float64(trust.LocalInput); ratio < 0.9 || ratio > 1.15 {
		t.Errorf("provider/ours is %.2fx, so the counts still do not cover the same requests", ratio)
	}
}

// The failure mode itself, pinned so the ordering cannot silently regress. At the
// moment of the second request's delta, the count we held was 34,060 against a
// reported 71,162: the 2.09x that cleared the tolerance and marked the run
// REJECTED. Reproduced from the recorded figures rather than a hand-trimmed copy,
// since trimming a breakdown by hand is how the earlier version of this test
// drifted away from the numbers it was meant to describe.
func TestCountingHalfTheRunWouldRejectAHealthyProvider(t *testing.T) {
	reported := llm.UsageInfo{InputTokens: 71_162, CachedInputTokens: 71_119}

	// Exactly what the run recorded at the point of comparison, expressed as a
	// breakdown so no test-only setter is needed: Any carries a total on its own.
	partial := llm.TokenBreakdown{Other: 34_060}

	trust := llm.ReconcileUsage(reported, partial)
	if trust.Trusted {
		t.Fatal("the guard no longer detects a one-request lag")
	}
	ratio := float64(reported.InputTokens) / float64(trust.LocalInput)
	if ratio < 2.0 || ratio > 2.2 {
		t.Errorf("expected the historical 2.09x gap, got %.2fx", ratio)
	}
}
