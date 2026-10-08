package llm

import "fmt"

// Provider token accounting cannot be trusted uniformly.
//
// Some endpoints report input_tokens as a running total for the conversation
// rather than the size of the request just sent. Because usage is accumulated
// across every request in a run, such a figure compounds: one live run reported
// 568x the prompt that was actually sent. Both cache ratio and cost are derived
// from InputTokens, so a number like that does not merely add noise, it invents
// the two figures an operator uses to decide whether a provider is worth using.
//
// We can catch it because we already count the prompt ourselves, in
// TokenBreakdown, from the same messages that were sent. Counting differs from
// the provider's tokenizer by a few percent, so agreement within a wide band
// means the figure is describing this request; a two-order-of-magnitude gap means
// it is describing something else entirely.

// UsageTrust records whether provider-reported usage can be believed, and why not.
type UsageTrust struct {
	Trusted bool   `json:"trusted"`
	Note    string `json:"note,omitempty"`
	// LocalInput is what we measured ourselves, kept so the discrepancy stays
	// visible in the session file rather than only in a log line.
	LocalInput int `json:"local_input,omitempty"`
	Reported   int `json:"reported,omitempty"`
}

// tokenAgreementLow and tokenAgreementHigh bound how far the provider's count may
// drift from ours before it is treated as describing a different quantity.
//
// Tokenizers legitimately disagree, and providers round differently, so the band
// is wide. A run that disagrees by less than this is trusted; the endpoints
// observed returning a cumulative total miss it by two orders of magnitude, so
// there is a large margin between "slightly different tokenizer" and "not this
// request at all".
const (
	tokenAgreementLow  = 0.5
	tokenAgreementHigh = 2.0
)

// ReconcileUsage compares our own prompt count against the provider's reported
// input tokens and decides whether the reported figure describes this request.
//
// local is summed across the same requests as usage, so the two are directly
// comparable. A zero local count means we could not measure, which is not
// evidence of a bad provider, so the figure is left trusted.
//
// Two conventions exist for where cached tokens are counted. Inclusive
// endpoints report the whole prompt in input_tokens; split endpoints (DeepSeek,
// native Anthropic) report only fresh tokens there and put the cached prefix in
// cached_input_tokens. Either candidate may be compared against our full-prompt
// count, and agreement on either one trusts the run: a running-total endpoint
// misses by two orders of magnitude on both, so the tripwire still holds.
func ReconcileUsage(usage UsageInfo, local TokenBreakdown) UsageTrust {
	localInput := local.Total()
	trust := UsageTrust{LocalInput: localInput, Reported: usage.InputTokens}

	if localInput <= 0 || usage.InputTokens <= 0 {
		trust.Trusted = true
		return trust
	}

	if withinAgreement(float64(usage.InputTokens)/float64(localInput)) ||
		withinAgreement(float64(usage.InputTokens+usage.CachedInputTokens)/float64(localInput)) {
		trust.Trusted = true
		return trust
	}

	ratio := float64(usage.InputTokens) / float64(localInput)
	trust.Trusted = false
	trust.Note = fmt.Sprintf("provider reported %d input (+%d cached) tokens, we sent %d (%.0fx); treating it as a running total, not this request",
		usage.InputTokens, usage.CachedInputTokens, localInput, ratio)
	return trust
}

func withinAgreement(ratio float64) bool {
	return ratio >= tokenAgreementLow && ratio <= tokenAgreementHigh
}

// CacheReadRatioFor returns the cache hit ratio only when the usage behind it is
// trustworthy.
//
// Dividing a possibly-cumulative cached count by a possibly-cumulative input
// count yields a ratio that drifts toward 1 on its own, so an untrusted figure
// does not merely add error, it manufactures a healthy-looking number. Callers
// get 0 and are expected to say so rather than print a percentage.
func (u UsageInfo) CacheReadRatioFor(trust UsageTrust) float64 {
	if !trust.Trusted {
		return 0
	}
	return u.CacheReadRatio()
}
