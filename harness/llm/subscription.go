package llm

import "strings"

// Subscription provider identifiers.
//
// A subscription provider authenticates with a credential held in the encrypted
// store rather than an API key in its profile. That difference matters to
// anything deciding whether a provider is configured: the profile has no
// api_key field to find, so a check that requires one reports a working
// subscription install as unconfigured.

// HermesSubscription identifies a Nous/Hermes subscription profile.
const HermesSubscription = "hermes"

// IsSubscription reports whether a profile authenticates by subscription rather
// than by an API key stored in the profile.
//
// One predicate for every such check. The distinction was previously spelled as
// an inline comparison against "hermes" at three separate sites, so a Codex
// install was treated as an unconfigured plain provider by all of them: doctor
// warned, the startup banner claimed onboarding was required, and /start sent an
// authenticated user back to the provider picker.
func IsSubscription(subscription string) bool {
	switch subscription {
	case HermesSubscription, CodexSubscription, XAISubscription:
		return true
	default:
		return false
	}
}

// HermesBaseURL is the Nous Portal inference endpoint.
//
// The Portal is not a public API, and Hermes is the only thing that knows this
// address: the value was taken from the Hermes source, where it is the
// DEFAULT_NOUS_INFERENCE_URL registry constant. Guessing an equivalent public
// URL would send requests to an endpoint that does not serve this credential.
const HermesBaseURL = "https://inference-api.nousresearch.com/v1"

// DefaultHermesModel is the model selected when Hermes is chosen without one.
const DefaultHermesModel = "hermes-4-405b"

// HermesModels lists the model IDs known to be served by the Nous Portal
// inference endpoint.
//
// Kept as a list so IsHermesModel can refuse anything else, the same way Codex
// does. The endpoint is closed and returns a hard 400 for a model it does not
// serve, so picking a plausible-looking ID is a guaranteed failure rather than a
// maybe.
var HermesModels = []string{
	"hermes-4-405b",
	"hermes-4-70b",
	"hermes-4-34b",
}

// IsHermesModel reports whether a model ID is known to work on the endpoint.
func IsHermesModel(model string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	for _, known := range HermesModels {
		if trimmed == known {
			return true
		}
	}
	return false
}

// XAISubscription identifies an xAI/Grok subscription profile.
//
// Distinct from the plain xAI API-key provider: this one authenticates with an
// OAuth credential from a SuperGrok or X Premium subscription and is served by
// the Responses API rather than chat/completions.
const XAISubscription = "xai"

// XaiBaseURL is the Grok inference endpoint.
//
// Read from the OpenClaw xAI plugin manifest rather than guessed. It is not the
// public xAI console URL that a plain API key uses, which is the kind of
// near-miss that produces an authenticated request answered by a 404.
const XaiBaseURL = "https://api.x.ai/v1"

// XaiModels lists the model IDs the xAI provider is known to serve, with their
// context windows.
//
// From the same manifest. The default is grok-4.7, which is also what OpenClaw
// selects when no model is named, so an account that works there works here.
var XaiModels = []struct {
	ID            string
	ContextWindow int
}{
	{"grok-4.7", 500_000},
	{"grok-4.6", 500_000},
	{"grok-4.5", 500_000},
	{"grok-build-0.1", 256_000},
	{"grok-4.3", 1_000_000},
	{"grok-4.20-0309-reasoning", 1_000_000},
	{"grok-4.20-0309-non-reasoning", 1_000_000},
}

// DefaultXaiModel is the model selected when xAI is chosen without one.
const DefaultXaiModel = "grok-4.7"

// XaiMaxOutputTokens is the output cap OpenClaw uses for xAI.
const XaiMaxOutputTokens = 64_000

// IsXaiModel reports whether a model ID is one the xAI endpoint is known to serve.
func IsXaiModel(model string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	for _, known := range XaiModels {
		if trimmed == known.ID {
			return true
		}
	}
	return false
}

// XaiContextWindow returns a model's context window, or 0 when it is unknown.
//
// Zero rather than a guess: an invented window either triggers compaction far
// too early or lets a request grow past what the endpoint accepts.
func XaiContextWindow(model string) int {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	for _, known := range XaiModels {
		if trimmed == known.ID {
			return known.ContextWindow
		}
	}
	return 0
}
