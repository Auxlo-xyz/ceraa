package freebuff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SessionError is a failed session request.
type SessionError struct {
	Message      string
	StatusCode   int
	RetryAfterMs int64
	ErrorCode    string
}

func (e *SessionError) Error() string { return e.Message }

// UnsupportedError marks an admission 404/405: the server predates the
// dedicated route. Callers must fail closed rather than fall back.
type UnsupportedError struct{ *SessionError }

// failureDisposition is how a poll loop should react to a failed request.
type failureDisposition int

const (
	// dispositionRetry means the request provably did not commit, so it is safe
	// to repeat.
	dispositionRetry failureDisposition = iota
	// dispositionStop means the request failed definitively; retrying will not
	// help.
	dispositionStop
	// dispositionUnknown means the request may already have mutated the session
	// and must not be repeated blindly.
	dispositionUnknown
)

// classifySessionFailure ports classifyFreebuffSessionRequestFailure. A POST may
// already have rotated the active instance, so it is retried ONLY on statuses
// produced before the mutation can commit. GET is read-only and retries normal
// transient failures.
func classifySessionFailure(method string, err error) failureDisposition {
	var sessionErr *SessionError
	isSessionErr := errors.As(err, &sessionErr)
	if method == http.MethodPost {
		if !isSessionErr {
			return dispositionUnknown
		}
		switch sessionErr.StatusCode {
		case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusServiceUnavailable:
			return dispositionRetry
		}
		if sessionErr.StatusCode >= 400 && sessionErr.StatusCode < 500 {
			return dispositionStop
		}
		return dispositionUnknown
	}
	// A network-layer failure (StatusCode 0) means nothing came back.
	if !isSessionErr {
		return dispositionUnknown
	}
	if sessionErr.StatusCode == 0 {
		// A read-only GET that never got an answer is always safe to repeat.
		return dispositionRetry
	}
	if sessionErr.StatusCode == http.StatusRequestTimeout ||
		sessionErr.StatusCode == http.StatusTooManyRequests ||
		sessionErr.StatusCode >= 500 {
		return dispositionRetry
	}
	return dispositionStop
}

// parseRetryAfterMs reads a Retry-After header in either seconds or HTTP-date
// form and returns milliseconds, or 0 when absent/unparseable.
func parseRetryAfterMs(value string, now time.Time) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds >= 0 {
		return int64(seconds * 1000)
	}
	if when, err := http.ParseTime(value); err == nil {
		if delta := when.Sub(now); delta > 0 {
			return int64(delta / time.Millisecond)
		}
	}
	return 0
}

// sessionRequest is one callFreebuffSession invocation.
type sessionRequest struct {
	method     string
	token      string
	instanceID string
	multi      bool
	// attemptID is the cli:-prefixed instance's bare attempt id. Non-empty only
	// in Desktop-style multi-session mode.
	attemptID string
	model     string
	// walletSpendLimit is 0 or the literal "session".
	walletSpendLimit string
	firstTabDiscount bool
	compact          bool
}

// callSession performs one session request, assembling headers exactly as the
// Freebuff CLI does.
func (c *Client) callSession(ctx context.Context, req sessionRequest) (SessionResponse, error) {
	endpoint := c.apiBase() + sessionPath
	if req.method == http.MethodPost {
		endpoint = c.apiBase() + sessionAdmissionPath
	} else if req.method == http.MethodDelete && req.attemptID != "" {
		endpoint = c.apiBase() + sessionAttemptPath
	}

	request, err := http.NewRequestWithContext(ctx, req.method, endpoint, nil)
	if err != nil {
		return SessionResponse{}, err
	}
	request.Header.Set("Authorization", "Bearer "+req.token)
	request.Header.Set("Accept", "application/json")
	// A timezone is a scheduling preference, never proof of country or access.
	if tz := localTimezone(); tz != "" {
		request.Header.Set(headerTimezone, tz)
	}
	if req.firstTabDiscount {
		request.Header.Set(headerFirstTabDiscount, "1")
	} else {
		request.Header.Set(headerFirstTabDiscount, "0")
	}
	multi := req.multi || req.attemptID != ""
	if multi {
		request.Header.Set(headerMultiSession, "1")
		request.Header.Set(headerPurchaseContinuity, "1")
		if req.attemptID != "" && req.method != http.MethodGet {
			request.Header.Set(headerDesktopAttempt, req.attemptID)
		}
		if req.method == http.MethodGet && req.instanceID != "" {
			request.Header.Set(headerHeartbeat, "1")
			// A bare heartbeat omits balances and quotas; a rich refresh must ask.
			if !req.compact {
				request.Header.Set(headerIncludeUnusedRateLim, "1")
			}
		}
	}
	// Never sent on a plain POST.
	if (multi || req.method != http.MethodPost) && req.instanceID != "" {
		request.Header.Set(headerInstance, req.instanceID)
	}
	if req.method == http.MethodGet && req.compact {
		request.Header.Set(headerCompactSession, "1")
	}
	if req.method == http.MethodPost {
		if req.model != "" {
			request.Header.Set(headerModel, req.model)
		}
		limit := req.walletSpendLimit
		if limit == "" {
			limit = "0"
		}
		request.Header.Set(headerWalletSpendLimit, limit)
	}

	client := c.http()
	if client.Timeout == 0 {
		// Per-request bound; the caller's context is authoritative for cancel.
		clone := *client
		clone.Timeout = RequestTimeout
		client = &clone
	} else if client.CheckRedirect == nil {
		// Callers that inject a bare client (tests, embedders) still get the
		// credential-preserving redirect behaviour.
		clone := *client
		clone.CheckRedirect = newSecureClient().CheckRedirect
		client = &clone
	}
	response, err := client.Do(request)
	if err != nil {
		return SessionResponse{}, &SessionError{
			Message:    fmt.Sprintf("freebuff session %s failed: %v", req.method, err),
			StatusCode: 0,
		}
	}
	defer response.Body.Close()
	body, readErr := readLimited(response)
	if readErr != nil {
		return SessionResponse{}, &SessionError{
			Message:    fmt.Sprintf("freebuff session %s failed to read response: %v", req.method, readErr),
			StatusCode: 0,
		}
	}

	// Admission 404/405 fails closed: never fall back to the legacy path.
	if req.method == http.MethodPost && (response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed) {
		return SessionResponse{}, &UnsupportedError{&SessionError{
			Message:    sessionUnsupportedMessage,
			StatusCode: response.StatusCode,
			ErrorCode:  "session_admission_unsupported",
		}}
	}
	// GET/DELETE 404 simply means no session.
	if response.StatusCode == http.StatusNotFound {
		return SessionResponse{Status: "none"}, nil
	}

	var parsed SessionResponse
	decoded := len(body) > 0 && json.Unmarshal(body, &parsed) == nil

	// 403 with a recognized status is data, not an error.
	if response.StatusCode == http.StatusForbidden && decoded {
		if parsed.Status == "country_blocked" || parsed.Status == "banned" {
			return parsed, nil
		}
	}
	// 409 on POST: a set of refusals that carry their own recovery.
	if response.StatusCode == http.StatusConflict && req.method == http.MethodPost && decoded {
		switch parsed.Status {
		case "model_locked", "model_unavailable", "premium_slot_taken",
			"purchase_claim_released", "purchase_in_use", "purchase_capacity",
			"first_tab_discount_changed", "consent_required":
			return parsed, nil
		}
	}
	// 429 on POST: quota refusals.
	if response.StatusCode == http.StatusTooManyRequests && req.method == http.MethodPost && decoded {
		switch parsed.Status {
		case "rate_limited", "spend_limited", "ip_capped":
			return parsed, nil
		}
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		errorCode := ""
		if decoded {
			errorCode = parsed.Error
		}
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return SessionResponse{}, &SessionError{
			Message:      fmt.Sprintf("freebuff session %s failed: %d %s", req.method, response.StatusCode, snippet),
			StatusCode:   response.StatusCode,
			RetryAfterMs: parseRetryAfterMs(response.Header.Get("Retry-After"), c.now()),
			ErrorCode:    errorCode,
		}
	}
	if !decoded {
		return SessionResponse{}, &SessionError{
			Message:    fmt.Sprintf("freebuff session %s returned an unreadable body", req.method),
			StatusCode: response.StatusCode,
		}
	}
	return parsed, nil
}
