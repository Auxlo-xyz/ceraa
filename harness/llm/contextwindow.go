package llm

import "fmt"

// ContextWindowSource records where an effective context limit came from, so a
// surprising budget can be explained from a log line rather than guessed at.
type ContextWindowSource string

const (
	// ContextFromProfile means the provider profile stated a limit. This is the
	// source of truth: the limit is a decision somebody made, and it survives a
	// model switch, an unreachable catalog and a catalog that advertises less.
	ContextFromProfile ContextWindowSource = "profile"
	// ContextFromCatalog means the model catalog corrected the window downward:
	// it advertised a smaller window than the limit in force, so the smaller one
	// won. The catalog never raises a limit, so this source is always a
	// correction and never the origin of a number.
	ContextFromCatalog ContextWindowSource = "catalog"
	// ContextFromDefault means nothing was configured at all and the fallback
	// governs the run.
	ContextFromDefault ContextWindowSource = "default"
)

// FallbackContextWindow is the budget used when no provider profile states a
// limit. It is the operative window for an unset profile, not a placeholder for
// the catalog to replace: the catalog may only correct a limit downward.
//
// 200k, which is what a served model takes rather than a cautious guess: there
// is nothing to protect against by assuming less, because a model that cannot
// take 200k is not one these providers field. So this is a default and not a
// floor in the older sense of a number held below the real window on purpose.
// A profile that states a limit overrides it, and a model the catalog knows to
// be smaller pulls it down, but nothing raises it: an unset limit means this
// number governs the run.
//
// It is kept rather than deleted because four callers need a budget at the moment
// nothing has stated one: the context builder with no explicit limit, the
// compactor's own default, the runner's answer before its first calibration, and
// the command display. A zero is not a safe substitute for any of them -
// BuildWithLimit(0) skips trimming entirely, so the provider would be handed an
// unbounded prompt, and compactAt reports zero, so compaction would never fire.
const FallbackContextWindow = 200000

// ResolveContextWindow picks the effective context limit for a model.
//
// The configured limit is the single source of truth: a nonzero value is a
// decision somebody made, and a zero means the fallback governs the run. The
// model catalog is a check on that decision, never the source of it. It may only
// lower the window, in two cases:
//
//   - the configured limit exceeds what the model actually advertises, which is a
//     typo or a mistaken entry (512k set for a 256k model), and the catalog
//     corrects it; and
//   - nothing was configured and the model advertises less than the fallback, so
//     the smaller real window keeps a small model from being handed a prompt it
//     cannot accept.
//
// The catalog may never raise the window. A profile that states less than the
// model supports is deliberately conservative - the Codex endpoint carries a
// local budget below any real window - and an unset limit must not be inflated by
// whatever the catalog advertises. Letting the catalog raise it is how a session
// sat at 137% of the window it was being displayed while the compactor, resolving
// the same number against a 1M catalog entry, correctly decided not to fire.
//
// The result is the window, not a budget carved out of it. An earlier version
// subtracted a safety margin here, and the compactor then subtracted a buffer of
// its own, so two flat reservations stacked: a 200k model compacted at 80% of its
// window and a 32k model at 12%. Headroom is an absolute requirement, not a
// proportional one, so it belongs with the code that needs it. Callers that
// assemble a request subtract output and tool tokens; the compactor reserves
// through ReserveContextTokens.
//
// The catalog is consulted even for models it has never heard of, and returns
// whatever it has, so the caller must pass metadata from the model it is actually
// running rather than a cached struct from another.
func ResolveContextWindow(configured int, meta ModelMetadata) (int, ContextWindowSource) {
	limit, source := FallbackContextWindow, ContextFromDefault
	if configured > 0 {
		limit, source = configured, ContextFromProfile
	}
	if meta.ContextWindow > 0 && meta.ContextWindow < limit {
		return meta.ContextWindow, ContextFromCatalog
	}
	return limit, source
}

// ContextWindowDecision is a resolved limit plus its provenance, for logging.
type ContextWindowDecision struct {
	Limit            int
	Source           ContextWindowSource
	Model            string
	AdvertisedWindow int
	Configured       int
}

// String renders the decision so a log line explains the budget in full.
func (d ContextWindowDecision) String() string {
	return fmt.Sprintf("context limit %d from %s (model %q, advertised window %d, profile limit %d)",
		d.Limit, d.Source, d.Model, d.AdvertisedWindow, d.Configured)
}
