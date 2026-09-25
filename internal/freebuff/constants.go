// Package freebuff implements native Freebuff authentication for Sooraa.
//
// The public Freebuff client is a device-code login against freebuff.com that
// yields an opaque bearer session token. That token authenticates a user
// against the hosted codebuff.com API; there is no refresh token and no local
// inference path. Base URLs are compile-time constants on purpose: honoring an
// environment override for the credential-bearing host would let any process
// that can set one variable redirect bearer tokens to an attacker.
package freebuff

import "time"

const (
	// DefaultWebBaseURL serves the device-code login endpoints. It does not
	// redirect.
	DefaultWebBaseURL = "https://freebuff.com"
	// DefaultAPIBaseURL is the hosted API host, used for /api/v1/me, hosted
	// session admission, and chat completions.
	//
	// The canonical www host matters. The bare codebuff.com host answers a 301
	// to www.codebuff.com, and Go (like every other HTTP client) strips the
	// Authorization header on a cross-host redirect — which would turn every
	// authenticated call into "Missing or invalid Authorization header" and
	// look like a revoked token. Released Freebuff binaries bake in the www
	// host for the same reason. See also the redirect guard in newSecureClient.
	DefaultAPIBaseURL = "https://www.codebuff.com"
	// DefaultInferenceBaseURL is the OpenAI-compatible root. The Freebuff
	// completions endpoint is /api/v1/chat/completions and Sooraa's transport
	// appends "/chat/completions" to whatever base it is given, so the
	// inference base must already include the /api/v1 prefix.
	DefaultInferenceBaseURL = DefaultAPIBaseURL + "/api/v1"

	// Path segments, kept as constants so no string concatenation can produce a
	// subtly different path than the Freebuff CLI uses.
	codePath       = "/api/auth/cli/code"
	statusPath     = "/api/auth/cli/status"
	logoutPath     = "/api/auth/cli/logout"
	mePath         = "/api/v1/me"
	completionsURL = "/api/v1/chat/completions"
)

const (
	// RequestTimeout bounds a single HTTP request, matching the CLI.
	RequestTimeout = 20 * time.Second
	// PollInterval is the delay between status polls.
	PollInterval = 5 * time.Second
	// PollTimeout ends polling after 5 minutes. The server-side code lives for
	// one hour, but the Freebuff CLI only waits 5 minutes, so we do too.
	PollTimeout = 5 * time.Minute

	// MaxAttempts counts the first try plus three retries.
	MaxAttempts = 4
	// RetryBaseDelay is the first backoff step; it doubles per attempt.
	RetryBaseDelay = 1000 * time.Millisecond
	// RetryMaxDelay caps the backoff.
	RetryMaxDelay = 10 * time.Second
	// RetryJitter is the fraction of the delay added as random jitter.
	RetryJitter = 0.3
)

// FreebuffCompletionsURL is the fully qualified hosted completions endpoint.
// Kept exported for the Day 2 session/completions adapter and for tests that
// need to point a local server at the exact path.
func FreebuffCompletionsURL() string { return DefaultAPIBaseURL + completionsURL }
