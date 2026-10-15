package coordinator

import (
	"encoding/json"
	"fmt"
	"os"

	"ceraa/harness/llm"
	"ceraa/internal/logger"
)

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
	// requests counts model requests, so the cache log can number them.
	requests int
}

// traceCache is on by default because the question it answers - why the cache is
// not being hit - cannot be answered from a run total, and costs one line per
// request. Turn it off with CERAA_TRACE_CACHE=0.
var traceCache = os.Getenv("CERAA_TRACE_CACHE") != "0"

func ratio(cached, input int) float64 {
	if input <= 0 {
		return 0
	}
	// Same split-reporting convention as UsageInfo.CacheReadRatio: cached can
	// only exceed input when the endpoint counts fresh tokens alone, so that
	// comparison selects the denominator here too.
	if cached > input {
		return float64(cached) / float64(input+cached) * 100
	}
	return float64(cached) / float64(input) * 100
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
func (a *runAccounting) Add(delta llm.UsageInfo, local llm.TokenBreakdown, raw *json.RawMessage) {
	// Per-request cache log. One line per model request, carrying the exact
	// figures the provider reported for that request alone, because the run total
	// cannot say *where* a prefix stops matching: 50% over a run is consistent
	// with every request missing half its context and with half the requests
	// missing all of it, and those need different fixes.
	a.requests++
	if traceCache && raw != nil {
		// The provider's own usage object, verbatim. Everything else in this log
		// is Ceraa's reading of it, and the reading is what is in question.
		// The raw field is last and may be empty, so it is appended rather
		// than formatted inline: a trailing space after req=N reads as a
		// dangling field.
		line := fmt.Sprintf("rawusage req=%d", a.requests)
		if truncated := truncateRaw(string(*raw)); truncated != "" {
			line += " " + truncated
		}
		logger.Info(line)
	}
	if traceCache {
		logger.Info(fmt.Sprintf(
			"cache req=%d input=%d cached=%d hit=%.1f%% local_total=%d sys=%d tools=%d conv=%d toolout=%d reasoning=%d",
			a.requests, delta.InputTokens, delta.CachedInputTokens,
			ratio(delta.CachedInputTokens, delta.InputTokens),
			local.System+local.Tools+local.User+local.Conversation+local.ToolOutput+local.Reasoning+local.Compaction+local.Other, local.System, local.Tools, local.Conversation, local.ToolOutput, local.Reasoning))
	}
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

// countRequestBeforeUse folds a request's prompt estimate into the running total
// used to reconcile provider-reported usage.
//
// The ordering is the entire point. The usage delta for a request arrives from
// inside the stream callback, and reconciliation compares that running usage
// against this total. Counting the request after its delta lands means the total
// is missing the request being reported on, so a healthy provider's summed
// figures are compared against a partial local count.
//
// This was live: a two-request run reported 71,162 input tokens against a local
// count of 34,060, a 2.09x gap that cleared the tolerance and marked the run
// REJECTED. The provider's own dashboard disproved it, its per-call rows summing
// 35,654 + 35,508 to exactly the 71,162 we reported, which placed the fault here.
//
// Extracted so the ordering is testable without driving the whole run loop, which
// is what let the mistake survive a green suite in the first place.
func countRequestBeforeUse(total *llm.TokenBreakdown, request llm.TokenBreakdown) {
	total.Add(request)
}

// truncateRaw keeps a usage payload on one log line without silently cutting a
// field off the end, where it would look like the provider had not sent it.
func truncateRaw(s string) string {
	if len(s) <= 700 {
		return s
	}
	return s[:700] + "...TRUNCATED"
}
