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

// FallbackContextWindow is the budget used when neither the provider profile nor
// the model catalog states a window.
//
// 200k, which is what a served model takes rather than a cautious guess: there
// is nothing to protect against by assuming less, because a model that cannot
// take 200k is not one these providers field. So this is a default and not a
// floor in the older sense of a number held below the real window on purpose.
// It is also rarely the number that decides a run - a profile that states a
// limit overrides it, and the catalog supplies the model's real window as soon
// as it has been refreshed.
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
// configured is the provider profile's limit, which is zero when unset. A nonzero
// value wins, because it encodes a decision somebody made. Otherwise the
// catalog's advertised window is used. Only when both are silent does the
// fallback apply.
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
	if configured > 0 {
		return configured, ContextFromProfile
	}
	if meta.ContextWindow > 0 {
		return meta.ContextWindow, ContextFromCatalog
	}
	return FallbackContextWindow, ContextFromDefault
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
