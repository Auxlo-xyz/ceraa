package hermesauth

import "strings"

// ClientUserAgent is the User-Agent the Hermes client identifies with.
//
// The Nous Portal and its inference endpoint sit behind a WAF that answers 403
// with a Cloudflare interstitial for a default client. Measured against
// inference-api.nousresearch.com, the same unauthenticated request returned:
//
//	Go-http-client/1.1  -> 403 Cloudflare "Attention Required" page
//	hermes-cli           -> 400 JSON from the API itself
//
// so the block is on the product token, not on the request. This is not a Ceraa
// quirk: the Hermes client's own provider base sets the same header, and its
// comment says the default urllib User-Agent draws 403 from providers behind a
// WAF. The failure mode is nasty because the 403 body is HTML, so it surfaces as
// a provider error carrying a web page rather than as an authentication failure.
//
// The value is the product token on its own, deliberately. The Hermes client
// appends its own version, and appends a version here would be a claim about a
// client we are not: the WAF keys on `hermes-cli`, not on a number.
const ClientUserAgent = "hermes-cli"

// HeaderUserAgent is the header the WAF reads.
const HeaderUserAgent = "User-Agent"

// HasClientHeaders reports whether a header set already carries the Hermes client
// identity, so a profile's own headers are neither overwritten nor doubled.
func HasClientHeaders(headers map[string]string) bool {
	if len(headers) == 0 {
		return false
	}
	value, ok := headers[HeaderUserAgent]
	return ok && strings.TrimSpace(value) != ""
}

// WithClientHeaders returns headers carrying the Hermes client identity.
//
// A header the profile set deliberately is kept: someone who has pinned their own
// User-Agent is not overridden, exactly as with the Cline gateway headers. The
// input map is not modified.
func WithClientHeaders(headers map[string]string) map[string]string {
	if HasClientHeaders(headers) {
		return headers
	}
	merged := make(map[string]string, len(headers)+1)
	for key, value := range headers {
		merged[key] = value
	}
	merged[HeaderUserAgent] = ClientUserAgent
	return merged
}

// RequestHeaders is the identity header set for an inference request.
func RequestHeaders() map[string]string {
	return map[string]string{HeaderUserAgent: ClientUserAgent}
}
