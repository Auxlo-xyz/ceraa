package coordinator

import (
	"testing"

	"ceraa/harness/llm"
)

// pricedMetadata is pricing that is current and has a real input rate, so a
// withheld cost can only be explained by the trust decision and not by missing
// price data.
func pricedMetadata() llm.ModelMetadata {
	return llm.ModelMetadata{
		PricingCurrent: true,
		Pricing: llm.ModelPricing{
			Input:  llm.TokenPrice{Base: "0.000003"},
			Output: llm.TokenPrice{Base: "0.000015"},
		},
	}
}

// The failure this exists for: a provider reported a running total, and the run was
// priced from it anyway, inventing a total of 4.28 USD for a request we can show
// was 112K tokens.
func TestARunTotalIsNotPricedWhenTheProviderReportsARunningTotal(t *testing.T) {
	acct := newRunAccounting(pricedMetadata(), true)

	local := llm.TokenBreakdown{System: 517, Tools: 1197, User: 655, Conversation: 26010, ToolOutput: 83985, Reasoning: 11}
	acct.Add(llm.UsageInfo{InputTokens: 47_513_540, OutputTokens: 65_176}, local, nil)

	if acct.Trust().Trusted {
		t.Fatal("a 423x discrepancy was accepted")
	}
	if acct.Cost().Known {
		t.Errorf("an untrustworthy run was priced: %+v", acct.Cost())
	}
	if acct.Cost().TotalUSD != 0 {
		t.Errorf("a dollar figure was invented: %v", acct.Cost().TotalUSD)
	}
}

// Usage itself is still recorded. The counts are wrong about the request, but
// knowing what the provider claimed is what makes the discrepancy diagnosable.
func TestUsageIsStillRecordedForAnUntrustworthyRun(t *testing.T) {
	acct := newRunAccounting(pricedMetadata(), true)
	local := llm.TokenBreakdown{ToolOutput: 100_000}
	acct.Add(llm.UsageInfo{InputTokens: 50_000_000}, local, nil)

	if acct.Usage().InputTokens != 50_000_000 {
		t.Errorf("the provider's claim was discarded: %d", acct.Usage().InputTokens)
	}
	if acct.Trust().Note == "" {
		t.Error("no reason recorded, so the discrepancy cannot be acted on")
	}
}

// The normal path must be untouched. If this fails, the accounting has broken
// ordinary runs to fix broken ones.
func TestAnAgreeingRunIsStillPriced(t *testing.T) {
	acct := newRunAccounting(pricedMetadata(), true)
	local := llm.TokenBreakdown{ToolOutput: 100_000}
	acct.Add(llm.UsageInfo{InputTokens: 101_000, OutputTokens: 500}, local, nil)

	if !acct.Trust().Trusted {
		t.Fatalf("an agreeing run was rejected: %s", acct.Trust().Note)
	}
	if !acct.Cost().Known || acct.Cost().TotalUSD <= 0 {
		t.Errorf("an agreeing run was not priced: %+v", acct.Cost())
	}
}

// Once a run is untrustworthy it stays that way. A later delta cannot repair a
// figure that was already a running total, and resuming pricing would mix invented
// and real money in one total.
func TestAnUntrustworthyRunStaysUnpriced(t *testing.T) {
	acct := newRunAccounting(pricedMetadata(), true)
	acct.Add(llm.UsageInfo{InputTokens: 50_000_000}, llm.TokenBreakdown{ToolOutput: 100_000}, nil)

	// A later request that agrees on its own.
	acct.Add(llm.UsageInfo{InputTokens: 1_000, OutputTokens: 10}, llm.TokenBreakdown{ToolOutput: 1_000}, nil)

	if acct.Cost().Known {
		t.Errorf("cost resumed after an untrustworthy delta: %+v", acct.Cost())
	}
}

// An unpriced run is not the same as an untrustworthy one, and the difference has
// to survive into the report.
func TestMissingPricingIsDistinctFromUntrustedCounts(t *testing.T) {
	unpriced := newRunAccounting(pricedMetadata(), false)
	unpriced.Add(llm.UsageInfo{InputTokens: 100_000}, llm.TokenBreakdown{ToolOutput: 100_000}, nil)
	if !unpriced.Trust().Trusted {
		t.Error("missing price data was reported as untrustworthy counts")
	}
	if unpriced.Cost().Known {
		t.Error("a run with no current pricing was reported as priced")
	}

	untrusted := newRunAccounting(pricedMetadata(), true)
	untrusted.Add(llm.UsageInfo{InputTokens: 100_000_000}, llm.TokenBreakdown{ToolOutput: 100_000}, nil)
	if untrusted.Trust().Trusted {
		t.Error("bad counts were reported as trustworthy")
	}
}

// A run with no pricing data can still be untrustworthy, and then the reason
// recorded must be the counts, not the absent price.
func TestUntrustworthyCountsWinOverAbsentPricing(t *testing.T) {
	acct := newRunAccounting(pricedMetadata(), false)
	acct.Add(llm.UsageInfo{InputTokens: 100_000_000}, llm.TokenBreakdown{ToolOutput: 100_000}, nil)
	if acct.Trust().Trusted {
		t.Error("bad counts passed because pricing happened to be missing too")
	}
}

// Usage accumulation must not be gated on trust at all. A first delta is always
// recorded, because with nothing accumulated yet there is nothing to compare
// against; the risk is a later delta being dropped once the run is already
// untrusted, which would make the recorded usage depend on when the check ran.
func TestUsageKeepsAccumulatingAfterARunGoesUntrusted(t *testing.T) {
	acct := newRunAccounting(pricedMetadata(), true)
	acct.Add(llm.UsageInfo{InputTokens: 50_000_000}, llm.TokenBreakdown{ToolOutput: 100_000}, nil)
	if acct.Trust().Trusted {
		t.Fatal("setup: the run should be untrusted")
	}

	acct.Add(llm.UsageInfo{InputTokens: 60_000_000, OutputTokens: 42}, llm.TokenBreakdown{ToolOutput: 100_000}, nil)

	if got := acct.Usage().InputTokens; got != 110_000_000 {
		t.Errorf("InputTokens = %d, want 110000000: usage stopped accumulating once the run went untrusted", got)
	}
	if got := acct.Usage().OutputTokens; got != 42 {
		t.Errorf("OutputTokens = %d, want 42", got)
	}
	if acct.Cost().Known {
		t.Errorf("cost appeared after a later delta: %+v", acct.Cost())
	}
}
