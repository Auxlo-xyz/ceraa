package oauthdevice

import (
	"fmt"
	"net/url"
	"strings"
)

// Dialect describes how a provider deviates from the OAuth 2.0 device flow as
// RFC 8628 and RFC 6749 define it.
//
// It exists because the two providers that both authenticate this way disagree on
// almost every field, and hiding that inside the poll loop as per-provider
// branches produces code where a reader cannot tell which parts are the standard
// and which parts are a quirk. Naming them makes each deviation a line of
// configuration, checked at construction rather than discovered at runtime.
//
// Every field is optional. A nil Dialect is the standard flow, which is what
// Nous and xAI both speak.
type Dialect struct {
	// CodeField names the value carried by the device-code response and echoed in
	// the poll request. Default "device_code"; MiniMax calls it "user_code".
	CodeField string

	// GrantType is sent in the poll request. Default is the device-code grant;
	// MiniMax uses its own user-code grant.
	GrantType string

	// ExpiryField names the field holding the token lifetime. Default
	// "expires_in".
	ExpiryField string

	// ExpiryAbsoluteMilliseconds marks the expiry as an absolute epoch timestamp
	// rather than a duration.
	//
	// This is not cosmetic. MiniMax returns 1790519361298, which read as seconds
	// is the year 58709, and a duration read as milliseconds is a fraction of a
	// second. Guessing either way produces a token that looks permanently valid or
	// permanently expired, and neither is recoverable by inspection afterwards.
	ExpiryAbsoluteMilliseconds bool

	// IntervalIsMilliseconds marks the poll interval as milliseconds. MiniMax
	// returns 3000, which read as seconds is a fifty minute wait between polls.
	IntervalIsMilliseconds bool

	// PendingStatusField names a field whose value means "still waiting". MiniMax
	// answers an unapproved poll with HTTP 200 and {"status":"pending"}, so a
	// standard reader treats it as a token response and reports a login that
	// succeeded and produced no access token.
	PendingStatusField string
	// PendingStatusValue is the value of PendingStatusField that means pending.
	PendingStatusValue string

	// ErrorStatusField and ErrorStatusCode read a failure out of a 2xx payload.
	// MiniMax wraps both success and failure in base_resp.
	ErrorStatusField string
	ErrorStatusValue int
	// ErrorMessageField is where the human-readable detail sits. It may be dotted
	// for the same reason ErrorStatusField may be.
	ErrorMessageField string

	// ExtraCodeParams are added to the device-code request. MiniMax requires a
	// PKCE challenge there, which RFC 8628 does not have.
	ExtraCodeParams url.Values

	// ExtraTokenParams are added to the token request.
	//
	// PKCE is a two-part secret and a provider that asks for the challenge on the
	// device-code request will also insist on the verifier when the token is
	// collected. MiniMax answers "code_verifier is required" without it, so
	// carrying only the challenge looks like a completed login that then produces
	// no credential, and the verifier is the half that was being dropped.
	ExtraTokenParams url.Values
}

// Standard is the flow as RFC 8628 and RFC 6749 define it, and the zero value
// means the same thing.
func Standard() Dialect {
	return Dialect{
		CodeField:         "device_code",
		GrantType:         GrantTypeDeviceCode,
		ExpiryField:       "expires_in",
		ErrorMessageField: "error_description",
	}
}

// withDefaults fills in the fields a dialect left empty, so every reader below can
// assume a value and a nil Dialect behaves as the standard flow.
func (d Dialect) withDefaults() Dialect {
	if d.CodeField == "" {
		d.CodeField = "device_code"
	}
	if d.GrantType == "" {
		d.GrantType = GrantTypeDeviceCode
	}
	if d.ExpiryField == "" {
		d.ExpiryField = "expires_in"
	}
	if d.ErrorMessageField == "" {
		d.ErrorMessageField = "error_description"
	}
	return d
}

// pending inspects a successful-status payload and reports whether it is really a
// "not yet" answer, or a failure wearing a 200.
func (d Dialect) pending(payload map[string]any) *PollError {
	if d.PendingStatusField != "" {
		if value, _ := payload[d.PendingStatusField].(string); value == d.PendingStatusValue {
			return &PollError{Code: "authorization_pending", Message: d.PendingStatusField + ": " + value}
		}
	}
	if d.ErrorStatusField != "" {
		// The path may be dotted, because a provider wraps the status in an
		// envelope: MiniMax answers {"base_resp":{"status_code":0,...}} on both
		// success and failure, so a flat lookup never finds it and every wrapped
		// failure reads as a success.
		code, ok := numericPath(payload, d.ErrorStatusField)
		if ok && code != d.ErrorStatusValue {
			detail := stringPath(payload, d.ErrorMessageField)
			if detail == "" {
				detail = "authorization failed"
			}
			return &PollError{Code: "provider_error", Message: detail}
		}
	}
	return nil
}

// stringPath reads a string field addressed by a dotted path.
func stringPath(payload map[string]any, path string) string {
	head, rest, nested := strings.Cut(path, ".")
	if !nested {
		value, _ := payload[head].(string)
		return value
	}
	inner, ok := payload[head].(map[string]any)
	if !ok {
		return ""
	}
	value, _ := inner[rest].(string)
	return value
}

// numericPath reads a numeric field addressed by a dotted path.
func numericPath(payload map[string]any, path string) (int, bool) {
	head, rest, nested := strings.Cut(path, ".")
	if !nested {
		return numericField(payload, head)
	}
	inner, ok := payload[head].(map[string]any)
	if !ok {
		return 0, false
	}
	return numericField(inner, rest)
}

// numericField reads a JSON number that may have arrived as a float or a string.
//
// MiniMax sends status_code as a number, but a host that stringifies its numerics
// is a routine difference, and a failed parse here would read as success.
func numericField(payload map[string]any, name string) (int, bool) {
	switch value := payload[name].(type) {
	case float64:
		return int(value), true
	case string:
		var parsed int
		if _, err := fmt.Sscan(value, &parsed); err == nil {
			return parsed, true
		}
	}
	return 0, false
}
