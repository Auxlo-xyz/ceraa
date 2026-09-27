package coordinator

import "ceraa/harness/llm"

// runAccounting holds one run's token totals and decides what may be concluded
// from them.
//
// This was a closure inside the run loop, which made the policy untestable: the
// rules that decide whether a run is priced at all are the rules an operator is
// relying on when they read a cost, so they need to be a named unit that can be
// exercised directly rather than inferred from an end-to-end run.
//
// The central decision is that untrustworthy counts yield no cost. Not a corrected
// cost, not a cost with a caveat attached: no figure. We know precisely what we
// sent, because we counted it, and nothing about what the provider's number meant,
// so any total built from it would be invented. Reporting nothing is the honest
// outcome and is also the cheap one to act on.
type runAccounting struct {
	usage   llm.UsageInfo
	cost    llm.CostInfo
	trust   llm.UsageTrust
	pricing llm.ModelMetadata
	// priced reports whether pricing data is current, so an unpriced run is not
	// confused with an untrustworthy one.
	priced bool
}

func newRunAccounting(pricing llm.ModelMetadata, priced bool) *runAccounting {
	return &runAccounting{
		trust:   llm.UsageTrust{Trusted: true},
		pricing: pricing,
		priced:  priced,
	}
}

// Add folds in one request's reported usage and prices it if the numbers hold up.
//
// The comparison is against the run's accumulated prompt count rather than this
// single delta, because that is the quantity the provider's figure describes when
// it is describing the right thing at all.
func (a *runAccounting) Add(delta llm.UsageInfo, local llm.TokenBreakdown) {
	a.usage.Add(delta)
	a.trust = llm.ReconcileUsage(a.usage, local)
	if !a.trust.Trusted {
		return
	}
	if a.priced {
		if cost, ok := a.pricing.Cost(delta); ok {
			a.cost.Add(cost)
		}
	}
}

func (a *runAccounting) Usage() llm.UsageInfo  { return a.usage }
func (a *runAccounting) Cost() llm.CostInfo    { return a.cost }
func (a *runAccounting) Trust() llm.UsageTrust { return a.trust }

// CacheReadRatio reports the hit rate only while the counts behind it hold up.
func (a *runAccounting) CacheReadRatio() float64 {
	return a.usage.CacheReadRatioFor(a.trust)
}
