package freebuff

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sync"

	"ceraa/internal/envs"
)

// cliUserAgentProduct is the product token the official Freebuff CLI sends, in
// the exact RFC 7231 form the server's normalizer expects: product/version.
// The server lowercases the product and keeps the version only when it looks
// like a version, so both halves are shaped deliberately.
//
// The operator has chosen to identify as the official CLI so Ceraa's traffic is
// not treated as an unrecognized client. That is a deliberate trade: it means a
// version string here is a claim about which client is talking, so keep
// cliUserAgentVersion honest and overridable rather than hardcoded to a value
// that silently rots.
const cliUserAgentProduct = "Freebuff-CLI"

// DefaultCLIVersion is the Freebuff CLI release this integration tracks. It is
// overridable with CERAA_FREEBUFF_CLI_VERSION.
const DefaultCLIVersion = "0.0.197"

// cliUserAgentVersion resolves the version to advertise. The server drops a
// version that is not version-shaped or is longer than 16 characters, so an
// unparseable override falls back to the tracked default rather than sending a
// value that would be discarded.
func cliUserAgentVersion() string {
	override := envs.Get("FREEBUFF_CLI_VERSION")
	if override == "" {
		return DefaultCLIVersion
	}
	if len(override) > 16 || !versionPattern.MatchString(override) {
		return DefaultCLIVersion
	}
	return override
}

// versionPattern mirrors the server's VERSION_RE: a leading digit followed by
// dotted/alphanumeric parts.
var versionPattern = regexp.MustCompile(`^\d[0-9A-Za-z.+-]*$`)

// completionsUserAgent is the User-Agent sent on hosted completions.
func completionsUserAgent() string {
	return cliUserAgentProduct + "/" + cliUserAgentVersion()
}

// idPrefix namespaces Ceraa's correlation ids. The reference reserves the
// "wf-" prefix for its own published proxy generator, so a distinct prefix keeps
// Ceraa's traffic unambiguous.
const idPrefix = "ceraa-"

// capacityDeferredCode is a 429 that means "try later", not "you are blocked".
const capacityDeferredCode = "free_mode_capacity_deferred"

// MetadataBuilder produces the codebuff_metadata block hosted completions
// require. run_id must be a run the server has actually registered, so it is
// supplied by SetRunID rather than minted locally; client_id is fresh per
// request. A builder with no registered run emits an empty run_id, which the
// completions route rejects, so callers must set one before streaming.
type MetadataBuilder struct {
	mu    sync.Mutex
	runID string
}

// NewMetadataBuilder returns a builder with no registered run yet.
func NewMetadataBuilder() *MetadataBuilder { return &MetadataBuilder{} }

// SetRunID installs the run id returned by the run-registration endpoint.
func (b *MetadataBuilder) SetRunID(runID string) {
	b.mu.Lock()
	b.runID = runID
	b.mu.Unlock()
}

// HasRun reports whether a server-registered run id is installed.
func (b *MetadataBuilder) HasRun() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runID != ""
}

// ResetRun clears the run id, used when a session ends and a new one is admitted.
func (b *MetadataBuilder) ResetRun() {
	b.mu.Lock()
	b.runID = ""
	b.mu.Unlock()
}

func (b *MetadataBuilder) currentRunID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runID
}

// newCorrelationID returns a namespaced random id.
func newCorrelationID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return idPrefix + "0"
	}
	return idPrefix + hex.EncodeToString(buf)
}

// Body returns the top-level body fields for one hosted completions request.
//
// The reference spreads providerOptions["codebuff"] into the request body, so
// codebuff_metadata is a TOP-LEVEL key, not a nested provider object. Reserved
// fields are written last so a caller can never override them.
func (b *MetadataBuilder) Body(instanceID string) map[string]any {
	metadata := map[string]any{
		// Caller-supplied keys first, mirroring the reference order.
		"freebuff_instance_id": instanceID,
		"run_id":               b.currentRunID(),
		"client_id":            newCorrelationID(),
		// Free mode: the whole free-tier enforcement regime applies, which is
		// what Ceraa wants. Declaring any other mode with a Freebuff agent is
		// the documented escalation shape.
		"cost_mode": "free",
	}
	return map[string]any{"codebuff_metadata": metadata}
}

// Headers returns the provider-required request headers.
func (b *MetadataBuilder) Headers() map[string]string {
	return map[string]string{"user-agent": completionsUserAgent()}
}

// MakeRecoverError returns a hook for ProviderConfig.RecoverError that turns a
// hosted session gate into the right recovery.
//
// A gate code ending the session means the row is gone. The only supported
// recovery is to re-admit on the SAME instance id, so the hook re-admits
// synchronously and asks for the request to be re-issued. A gate code that does
// not end the session is terminal for this request and is reported in plain
// language instead of retried blindly — retrying a withdrawn model is what cost
// a Freebuff tier 2.5x its admissions.
func MakeRecoverError(sessions *SessionManager) func(ctx context.Context, status int, body []byte) (bool, string) {
	return func(ctx context.Context, status int, body []byte) (bool, string) {
		code := completionErrorCode(body)
		if code == "" {
			return false, ""
		}
		gate, ok := LookupGate(code, status)
		if !ok {
			return false, ""
		}
		if !gate.EndsTheSession {
			return false, gateMessage(code)
		}
		if sessions == nil {
			return false, "Freebuff session ended and Ceraa cannot re-admit it."
		}
		if err := sessions.ReAdmit(ctx); err != nil {
			return false, "Freebuff session ended and could not be re-admitted: " + err.Error()
		}
		return true, ""
	}
}

// completionErrorCode extracts the machine-readable error code from a
// completions error body. The gate is only meaningful together with the HTTP
// status, so the code alone is not treated as a rejection here.
func completionErrorCode(body []byte) string {
	var payload struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	if payload.Error != "" {
		return payload.Error
	}
	return payload.Code
}

// gateMessage renders a non-session-ending gate in plain language.
func gateMessage(code string) string {
	switch code {
	case "model_unavailable":
		return "That Freebuff model was withdrawn from free mode. Pick another model."
	case "session_limit_reached":
		return "This Freebuff account is at its concurrent-session limit."
	case "waiting_room_queued":
		return "Freebuff is queuing this request. Try again shortly."
	}
	return ""
}

// CapacityDeferred reports whether an error is a transient capacity deferral
// (HTTP 429 with free_mode_capacity_deferred), which is a "come back later"
// signal rather than a quota exhaustion.
func CapacityDeferred(status int, body []byte) bool {
	return status == 429 && completionErrorCode(body) == capacityDeferredCode
}

// DescribeError turns a hosted completions failure into a user-facing message,
// preferring the session/quota explanation over a raw provider body.
func DescribeError(status int, body []byte) string {
	code := completionErrorCode(body)
	if code == capacityDeferredCode {
		return "Freebuff is at capacity right now. Try again in a moment."
	}
	if gate, ok := LookupGate(code, status); ok {
		switch code {
		case "waiting_room_required":
			return "Freebuff wants this request to wait for a session slot."
		case "session_expired":
			return "The Freebuff session expired. Ceraa will re-admit on the next message."
		case "session_superseded":
			return "Another client took over the Freebuff session."
		case "session_model_mismatch":
			return "The Freebuff session is on a different model. Ceraa will re-admit."
		case "session_limit_reached":
			return gateMessage(code)
		case "waiting_room_queued":
			return gateMessage(code)
		case "model_unavailable":
			return gateMessage(code)
		}
		_ = gate
	}
	if status == 401 || status == 403 {
		return "Freebuff rejected the credential. Run /freebuff-login again."
	}
	if status == 429 {
		return "Freebuff rate-limited this request. Try again shortly."
	}
	return fmt.Sprintf("Freebuff request failed (HTTP %d).", status)
}
