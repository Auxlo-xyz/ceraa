package llm

import "fmt"

// ContextWindowSource records where an effective context limit came from, so a
// surprising budget can be explained from a log line rather than guessed at.
type ContextWindowSource string

const (
	// ContextFromProfile means the provider profile stated a limit. Preferred
	// over the catalog because some limits are deliberately conservative: the
	// Codex endpoint does not advertise a window, so its profile carries a local
	// budget that is smaller than any real window and should not be overridden.
	ContextFromProfile ContextWindowSource = "profile"
	// ContextFromCatalog means the model catalog supplied the window.
	ContextFromCatalog ContextWindowSource = "catalog"
	// ContextFromDefault means nothing was known and a conservative fallback was
	// used.
	ContextFromDefault ContextWindowSource = "default"
)

// FallbackContextWindow is the budget used when neither the profile nor the
// catalog states a window.
//
// Deliberately conservative rather than matching the largest window seen in
// practice. Over-budgeting a model that cannot take it produces a hard provider
// error and a lost turn; under-budgeting one that can merely wastes headroom. The
// two failures are not symmetric, so the default resolves toward the survivable
// one.
const FallbackContextWindow = 128000

// ContextSafetyMargin is the most that is held back from a catalog-advertised
// window for output, tool definitions and estimation error. It is the same
// allowance the previous hardcoded budget used when it took 256k and called the
// result 236k, and it is a ceiling rather than a fixed deduction: for a window
// small enough that the full margin would dominate it, a quarter of the window is
// used instead.
const ContextSafetyMargin = 20000

// maxReserveDivisor caps the reserve at this fraction of the window, so a
// resolved budget is always at least three quarters of what the model accepts.
const maxReserveDivisor = 4

// ResolveContextWindow picks the effective context limit for a model.
//
// configured is the provider profile's limit, which is zero when unset. A nonzero
// value wins, because it encodes a decision somebody made. Otherwise the
// catalog's advertised window is used, less the safety margin. Only when both are
// silent does the fallback apply.
//
// The catalog is consulted even for models it has never heard of, and returns
// whatever it has, so the caller must pass metadata from the model it is actually
// running rather than a cached struct from another.
func ResolveContextWindow(configured int, meta ModelMetadata) (int, ContextWindowSource) {
	if configured > 0 {
		return configured, ContextFromProfile
	}
	window := meta.ContextWindow
	if window <= 0 {
		return FallbackContextWindow, ContextFromDefault
	}
	// The reserve scales down for a small window rather than being subtracted
	// flat. A flat margin turns a 20,001-token window into a 1-token budget,
	// which is worse than no budget at all: the first turn fails and the failure
	// looks like a provider error rather than a budgeting bug.
	margin := ContextSafetyMargin
	if window/maxReserveDivisor < margin {
		margin = window / maxReserveDivisor
	}
	return window - margin, ContextFromCatalog
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
