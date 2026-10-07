package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// Exchange is one recorded request and its response.
//
// It is a record, not a live request. Nothing in this package opens a socket,
// which is what lets the whole spine be tested without a network and what keeps
// a finding check from becoming a scanner.
type Exchange struct {
	// Label names the exchange's role in the proof: the payload attempt, the
	// baseline, the control, the anonymous probe. A control that cannot be tied
	// to its role is a control nobody can audit.
	Label string `json:"label"`
	// Method is the HTTP method used.
	Method string `json:"method,omitempty"`
	// URL is the request target.
	URL string `json:"url,omitempty"`
	// Status is the response status, or 0 when there was no response.
	Status int `json:"status,omitempty"`
	// RequestHeaders are the headers sent, with any credential replaced by a
	// placeholder. A trace that is useful is one that can be shown.
	RequestHeaders map[string]string `json:"request_headers,omitempty"`
	// ResponseHeaders are the headers received.
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	// Body is the response body, capped by whoever recorded it.
	Body string `json:"body,omitempty"`
	// Observations are side-channel events seen while this exchange was in
	// flight: a dialog that fired, a console line, a callback that arrived.
	//
	// This is how execution is told apart from reflection. A reflected payload is
	// in Body; a payload that executed is in Observations. Conflating the two is
	// the single most common way an XSS finding is invented.
	Observations []Observation `json:"observations,omitempty"`
	// Error is why there is no response, when there is none.
	Error string `json:"error,omitempty"`
}

// Observation is one recorded side-channel event.
type Observation struct {
	// Kind is the channel, for example "exec:dialog", "exec:console",
	// "oob:callback", "navigation:location".
	Kind string `json:"kind"`
	// Detail is the value that carried the marker.
	Detail string `json:"detail,omitempty"`
}

// Bundle is the evidence a finding was first recorded with, plus what happened
// when it was re-derived.
//
// Both halves are required for a Proven verdict. One alone is a claim; the pair
// is a check.
type Bundle struct {
	// Original is what was captured when the finding was made.
	Original Exchange `json:"original"`
	// Fresh is what came back when it was independently re-run. It is the zero
	// value when no re-derivation has happened.
	Fresh Exchange `json:"fresh"`
	// FreshAttempted distinguishes "never re-run" from "re-ran and got nothing",
	// which Decide reports very differently.
	FreshAttempted bool `json:"fresh_attempted"`
	// FreshReachable is whether the re-run got a response.
	FreshReachable bool `json:"fresh_reachable"`
}

// HasBundle reports whether there is anything to check at all.
func (b Bundle) HasBundle() bool {
	return strings.TrimSpace(b.Original.Body) != "" || b.Original.Status != 0 || len(b.Original.Observations) > 0
}

// MarkerIn reports whether a marker appears in an exchange, counting the
// observation channels as well as the body and the headers.
//
// Counting observations is deliberate: an execution channel is the strongest
// evidence available and it is not in the body.
func MarkerIn(ex Exchange, marker string) bool {
	if strings.TrimSpace(marker) == "" {
		return false
	}
	if strings.Contains(ex.Body, marker) {
		return true
	}
	for _, observation := range ex.Observations {
		if strings.Contains(observation.Detail, marker) {
			return true
		}
	}
	for _, value := range ex.ResponseHeaders {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

// Rederivation is Bundle expressed as the evidence about a check that Decide
// consumes.
func (b Bundle) Rederivation(marker string) Rederivation {
	rd := Rederivation{
		Attempted:        b.FreshAttempted,
		Reachable:        b.FreshReachable,
		MarkerAvailable:  strings.TrimSpace(marker) != "",
		MarkerInBaseline: MarkerIn(b.Original, marker),
		MarkerPresent:    MarkerIn(b.Fresh, marker),
		Status:           b.Fresh.Status,
	}
	if b.Fresh.Error != "" {
		rd.Detail = b.Fresh.Error
	} else if rd.MarkerPresent {
		rd.Detail = "marker present on re-derivation"
	}
	return rd
}

// Decide runs the spine over a bundle and a marker.
func (b Bundle) Decide(marker string) Decision {
	return Decide(b.HasBundle(), b.Rederivation(marker))
}

// ---------------------------------------------------------------------------
// Fingerprint
// ---------------------------------------------------------------------------

// Fingerprint is a finding's identity across runs.
//
// Line numbers are deliberately excluded. Including them means that an unrelated
// edit anywhere above a finding mints a new identity, and a re-scan then reports
// every finding as new. That trains someone to ignore the tool, which is worse
// than having no tool.
//
// The three inputs are the class, the normalised path and a whitespace-collapsed
// snippet. Two genuinely different bugs in one file therefore keep separate
// identities, while the same bug before and after an unrelated refactor does not
// gain a new one.
func Fingerprint(class, path, snippet string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		strings.ToLower(strings.TrimSpace(class)),
		normalisePath(path),
		collapseSpace(snippet),
	}, "|")))
	return hex.EncodeToString(sum[:])[:12]
}

var pathSeparators = regexp.MustCompile(`[\\/]+`)

// normalisePath collapses the ways one file gets spelled.
//
// Leading and trailing separators are trimmed, not merely normalised, because a
// finding recorded from a shell and the same finding recorded from a source tree
// routinely differ by a leading slash. Without this the two are different bugs
// and a re-scan reports every finding as new.
func normalisePath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	cleaned := pathSeparators.ReplaceAllString(trimmed, "/")
	cleaned = strings.TrimPrefix(cleaned, "./")
	cleaned = strings.TrimPrefix(cleaned, "/")
	return strings.TrimSuffix(cleaned, "/")
}

func collapseSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// ---------------------------------------------------------------------------
// Trace helpers
// ---------------------------------------------------------------------------

// HeaderValue returns a response header value, case-insensitively.
//
// HTTP header names are case-insensitive and a recorded trace carries whatever
// capitalisation the server sent, so an exact map lookup silently returns "" and
// turns a real finding into "no Location header". That is a false negative
// dressed as a check, which is the one failure mode this package exists to
// avoid.
func (e Exchange) HeaderValue(name string) string {
	if e.ResponseHeaders == nil {
		return ""
	}
	if value, ok := e.ResponseHeaders[name]; ok {
		return value
	}
	keys := make([]string, 0, len(e.ResponseHeaders))
	for key := range e.ResponseHeaders {
		if strings.EqualFold(key, name) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		return e.ResponseHeaders[keys[0]]
	}
	return ""
}

// Observed reports whether an exchange produced an observation of a kind. A
// prefix match is used so "exec" covers "exec:dialog" and "exec:console".
func (e Exchange) Observed(kind string) bool {
	for _, observation := range e.Observations {
		if observation.Kind == kind || strings.HasPrefix(observation.Kind, kind+":") {
			return true
		}
	}
	return false
}

// Role returns the exchange carrying a label, and whether it was found.
func (b Bundle) Role(label string) (Exchange, bool) {
	if strings.EqualFold(b.Original.Label, label) {
		return b.Original, true
	}
	if strings.EqualFold(b.Fresh.Label, label) {
		return b.Fresh, true
	}
	return Exchange{}, false
}
