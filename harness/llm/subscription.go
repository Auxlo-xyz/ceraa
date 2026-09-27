package llm

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
	case HermesSubscription, CodexSubscription:
		return true
	default:
		return false
	}
}
