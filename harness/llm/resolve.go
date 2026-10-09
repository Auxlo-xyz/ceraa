package llm

import (
	"context"
	"strings"
	"time"
)

// Thinking is a model's reasoning capability, resolved from the catalog when the
// model was added. It answers the two questions a request needs answered: which
// effort words this model accepts, and what it does when none is sent.
type Thinking struct {
	// Efforts is the declared vocabulary, most first. Empty means the model
	// declared no reasoning, or the catalog has never been asked.
	Efforts []string
	// Default is the effort the model falls back to when the caller sends none.
	Default string
	// Mandatory means the model always thinks.
	Mandatory bool

	// Known reports whether the catalog was consulted and answered. It is what
	// keeps an unresolved model on the old behaviour instead of on a guess.
	Known bool
}

// Clamps reports whether a requested level may be sent as-is. An unknown
// vocabulary is passed through untouched, because the alternative is guessing
// narrower than the model actually is.
func (t Thinking) Allows(level ReasonLevel) bool {
	if !t.Known || len(t.Efforts) == 0 || level == "" {
		return true
	}
	requested := string(level)
	for _, effort := range t.Efforts {
		if effort == requested {
			return true
		}
	}
	return false
}

// Resolved returns the level to actually send for a request: the requested one
// when the model accepts it, otherwise the nearest effort it does accept, and
// its own default when it accepts none of Ceraa's levels at all.
func (t Thinking) Resolved(level ReasonLevel) ReasonLevel {
	if !t.Known || len(t.Efforts) == 0 {
		return level
	}
	if t.Allows(level) {
		return level
	}
	if t.Allows(ReasonLevel(t.Default)) {
		return ReasonLevel(t.Default)
	}
	return ReasonLevel(t.Efforts[0])
}

// resolveTimeout bounds one catalog lookup. It runs inside a save path, so a
// catalog that hangs must not hang the provider it is being asked about.
const resolveTimeout = 5 * time.Second

// ResolveThinking asks the model catalog what a model's thinking parameters
// are, over the endpoint the caller already uses for pricing.
//
// The same URL, cache and refresh interval are passed in rather than defaulted,
// so a deployment pointed at a mirror or an offline cache resolves there too.
// A model the catalog does not list, which is every subscription model served
// from a credential rather than a public listing, resolves to Known=false and
// leaves the level alone: the caller keeps whatever it had, exactly as before
// this existed.
func ResolveThinking(ctx context.Context, catalogURL, cachePath string, refresh time.Duration, model string) (Thinking, error) {
	trimmed := NormalizeCatalogModel(model)
	if trimmed == "" || strings.TrimSpace(catalogURL) == "" {
		return Thinking{}, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	client := NewCatalogClient(catalogURL, cachePath, refresh)
	metadata, err := client.Lookup(lookupCtx, trimmed)
	if err != nil {
		return Thinking{}, err
	}
	return Thinking{
		Efforts:   append([]string(nil), metadata.Reasoning.SupportedEfforts...),
		Default:   metadata.Reasoning.DefaultEffort,
		Mandatory: metadata.Reasoning.Mandatory,
		Known:     true,
	}, nil
}
