package freebuff

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
)

// completionsUserAgent mirrors the UA the Freebuff SDK sends on hosted
// completions. It identifies the client stack, not the Freebuff CLI: the
// official CLI's own "Freebuff-CLI/<version>" UA is deliberately NOT sent.
const completionsUserAgent = "ai-sdk/openai-compatible/1.0/sooraa"

// idPrefix namespaces Sooraa's correlation ids. The reference reserves the
// "wf-" prefix for its own published proxy generator, so a distinct prefix keeps
// Sooraa's traffic unambiguous.
const idPrefix = "sooraa-"

// capacityDeferredCode is a 429 that means "try later", not "you are blocked".
const capacityDeferredCode = "free_mode_capacity_deferred"

// MetadataBuilder produces the codebuff_metadata block hosted completions
// require. run_id identifies a run and stays stable for the life of the builder;
// client_id is fresh per request.
type MetadataBuilder struct {
	mu    sync.Mutex
	runID string
}

// NewMetadataBuilder starts a builder with a fresh run id.
func NewMetadataBuilder() *MetadataBuilder {
	return &MetadataBuilder{runID: newCorrelationID()}
}

// ResetRun rotates the run id, used when a run ends.
func (b *MetadataBuilder) ResetRun() {
	b.mu.Lock()
	b.runID = newCorrelationID()
	b.mu.Unlock()
}

func (b *MetadataBuilder) currentRunID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runID == "" {
		b.runID = newCorrelationID()
	}
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
		// what Sooraa wants. Declaring any other mode with a Freebuff agent is
		// the documented escalation shape.
		"cost_mode": "free",
	}
	return map[string]any{"codebuff_metadata": metadata}
}

// Headers returns the provider-required request headers.
func (b *MetadataBuilder) Headers() map[string]string {
	return map[string]string{"user-agent": completionsUserAgent}
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
			return false, "Freebuff session ended and Sooraa cannot re-admit it."
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
			return "The Freebuff session expired. Sooraa will re-admit on the next message."
		case "session_superseded":
			return "Another client took over the Freebuff session."
		case "session_model_mismatch":
			return "The Freebuff session is on a different model. Sooraa will re-admit."
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
