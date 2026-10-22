package verify

// controls.go is the control machinery: the questions a web finding has to
// answer, and the obligations that answer them.
//
// Every control answers the same question in a different way: what would a
// naive check have accepted here that is not this bug? The controls below
// are the ones that catch the mistakes a validator makes most often, and
// each of them carries the obligation it enforces so that a reader who
// disagrees with a result can say which part of it they disagree with.
//
// Two of them are the load-bearing pair:
//
//   - the marker guard, which refuses a canary the baseline already carried.
//     A validator that fetches a fresh response, finds its own marker and
//     declares success has proved that the server echoes input, which is
//     true of essentially every web server and is not a vulnerability.
//   - the reflection control, which refuses a response that differs from the
//     baseline by nothing but the echoed payload. That is what a web server
//     does with a parameter it does not understand.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// minCanaryLength is the shortest value the marker guard accepts as a canary.
//
// A canary is a value the tester chose to be recognisable. A one-character
// parameter value is not a canary: it appears in most responses, so matching
// it says nothing about the request that carried it. The redactor uses the
// same floor for the same reason.
const minCanaryLength = 8

// MarkerControl builds the control that proves an injected value changed the
// response.
//
// The obligation has two halves and both of them are needed. The marker has
// to be in the recorded response, and the baseline - the same request without
// the payload - must not carry it. A marker the baseline carried as well
// proves only that the server echoes input.
//
// A missing baseline is Missing and not Established. The anchoring half of
// the check cannot run without it, and a check that did not run must not be
// reported as a check that passed.
func MarkerControl(marker string) Control {
	return Control{
		Name:     "marker",
		Question: "the marker is in the recorded response and the baseline did not carry it",
		Enforce: func(o Observation) Obligation {
			needle := strings.TrimSpace(marker)
			if needle == "" {
				return missing("the control names no marker to look for")
			}
			if o.ResponseBody == "" {
				return missing("the recorded response carries no body, so there is nothing to look for")
			}
			if !strings.Contains(o.ResponseBody, needle) {
				return contradicted("the marker is not in the recorded response")
			}
			if o.Baseline == nil {
				return missing("there is no baseline, so the marker cannot be anchored to the original response")
			}
			if markerInBaseline(needle, o.Baseline) {
				return contradicted("the baseline carries the marker too, so the echo proves nothing")
			}
			return established(fmt.Sprintf("the marker %q is in the recorded response and the baseline did not carry it", needle))
		},
	}
}

// markerInBaseline reports whether the baseline response already carried the
// marker.
//
// This is the false-Proven guard. A validator that fetches a fresh response,
// finds its own canary and declares success has proved the server echoes
// input. The marker has to have been absent from the original recorded
// response too, or the echo proves nothing.
func markerInBaseline(marker string, baseline *Observation) bool {
	if baseline == nil || marker == "" {
		return false
	}
	return strings.Contains(baseline.ResponseBody, marker)
}

// MarkerGuardControl builds the marker control for a canary the caller does
// not know in advance: the values the request carried.
//
// The request is the record of what the tester sent, so a value that comes
// back in the response but not in the baseline is a canary that made the
// round trip. The baseline comparison is the same guard MarkerControl
// applies, and it is what keeps a server that echoes input from passing.
//
// Only a value long enough to be a canary counts. A short parameter value
// appears in most responses and would make the guard pass on nothing.
func MarkerGuardControl() Control {
	return Control{
		Name:     "marker",
		Question: "a value the request carried is in the recorded response and the baseline did not carry it",
		Enforce: func(o Observation) Obligation {
			if o.ResponseBody == "" {
				return missing("the recorded response carries no body, so there is nothing to look for")
			}
			candidates := canaryValues(o)
			if len(candidates) == 0 {
				return missing("the request carried no value long enough to be a canary")
			}
			for _, candidate := range candidates {
				if !strings.Contains(o.ResponseBody, candidate) {
					continue
				}
				if o.Baseline == nil {
					return missing("there is no baseline, so the marker cannot be anchored to the original response")
				}
				if markerInBaseline(candidate, o.Baseline) {
					return contradicted("the baseline carries the value too, so the echo proves nothing")
				}
				return established(fmt.Sprintf("the value %q came back in the recorded response and the baseline did not carry it", candidate))
			}
			return contradicted("no value the request carried appears in the recorded response")
		},
	}
}

// StatusControl builds the control that the response status is not the one a
// public page returns.
//
// A 200 is what a public page returns, so a status that matches the baseline
// proves nothing. An expected status, when one is named, is the second way
// the obligation can be met: a status the finding expects is evidence even
// when the baseline agrees with it.
//
// A baseline without a status is Missing. There is nothing to compare
// against, and "the status is 200" on its own is the answer a public page
// gives.
func StatusControl(expected ...int) Control {
	return Control{
		Name:     "status",
		Question: "the status is not the one the baseline returned",
		Enforce: func(o Observation) Obligation {
			if o.Status == 0 {
				return missing("the recorded exchange carries no status")
			}
			if o.Baseline != nil && o.Baseline.Status != 0 && o.Status != o.Baseline.Status {
				return established(fmt.Sprintf("the status is %d and the baseline returned %d", o.Status, o.Baseline.Status))
			}
			for _, want := range expected {
				if want == o.Status {
					return established(fmt.Sprintf("the status is %d, which is the status the finding expects", o.Status))
				}
			}
			if o.Baseline == nil || o.Baseline.Status == 0 {
				return missing("there is no baseline status to compare the recorded status against")
			}
			return contradicted(fmt.Sprintf("the status is %d, the same as the baseline: a status a public page already returns is not evidence of this bug", o.Status))
		},
	}
}

// BodyChangeControl builds the control that the response body is not the one
// the baseline returned.
//
// The body is where the finding lives, so an unchanged body is the clearest
// possible refutation: the request changed nothing.
func BodyChangeControl() Control {
	return Control{
		Name:     "body",
		Question: "the response body is not the one the baseline returned",
		Enforce: func(o Observation) Obligation {
			if o.ResponseBody == "" && (o.Baseline == nil || o.Baseline.ResponseBody == "") {
				return missing("neither the recorded response nor the baseline carries a body")
			}
			if o.Baseline == nil {
				return missing("there is no baseline body to compare the recorded body against")
			}
			if o.ResponseBody == o.Baseline.ResponseBody {
				return contradicted("the response body is identical to the baseline")
			}
			return established(fmt.Sprintf("the response body is %d bytes and the baseline body is %d bytes", len(o.ResponseBody), len(o.Baseline.ResponseBody)))
		},
	}
}

// ReflectionControl builds the control that the response shows more than
// the payload echoed back.
//
// A parameter that comes back in the response has proved nothing: every web
// server prints back what it was given. The obligation is met only when the
// response differs from the baseline by something the echoed payload does
// not explain.
//
// The comparison is made on the response with the echoes removed, and the
// whitespace is collapsed on both sides, because a server that prints the
// payload into a template rarely reproduces the rest of the page byte for
// byte.
func ReflectionControl() Control {
	return Control{
		Name:     "reflection",
		Question: "the response shows more than the payload echoed back",
		Enforce: func(o Observation) Obligation {
			if o.ResponseBody == "" {
				return missing("the recorded response carries no body")
			}
			if o.Baseline == nil {
				return missing("there is no baseline to compare the response against")
			}
			candidates := canaryValues(o)
			if len(candidates) == 0 {
				return missing("the request carried no payload long enough to tell an echo from a change")
			}
			echoes := make([]string, 0, len(candidates))
			for _, candidate := range candidates {
				if strings.Contains(o.ResponseBody, candidate) && !strings.Contains(o.Baseline.ResponseBody, candidate) {
					echoes = append(echoes, candidate)
				}
			}
			if len(echoes) == 0 {
				return contradicted("no value the request carried came back that the baseline did not already carry")
			}
			stripped := stripAll(o.ResponseBody, echoes)
			if collapseSpace(stripped) == collapseSpace(o.Baseline.ResponseBody) {
				return contradicted("the response differs from the baseline by nothing but the echoed payload")
			}
			return established("the response differs from the baseline by more than the echoed payload")
		},
	}
}

// SecretControl builds the control that the response carries a
// credential-shaped value.
//
// The value is masked before it reaches the obligation. A finding that quotes
// a credential in full is the one place Ceraa's own redaction guarantee
// would be defeated.
func SecretControl() Control {
	return Control{
		Name:     "secret",
		Question: "the response carries a credential-shaped value",
		Enforce: func(o Observation) Obligation {
			if o.ResponseBody == "" && len(o.ResponseHeaders) == 0 {
				return missing("the recorded response carries nothing to scan")
			}
			if match, ok := FindSecret(o.ResponseBody); ok {
				return established(fmt.Sprintf("the response body carries a credential of the shape %q: %s", match.Pattern, match.Masked))
			}
			for _, name := range sortedKeys(o.ResponseHeaders) {
				if match, ok := FindSecret(o.ResponseHeaders[name]); ok {
					return established(fmt.Sprintf("the response header %q carries a credential of the shape %q: %s", name, match.Pattern, match.Masked))
				}
			}
			return contradicted("the response carries no credential-shaped value")
		},
	}
}

// DefaultControls returns the controls that cover the common web findings.
//
// The set is deliberately small and every member answers the same question:
// what would a naive check have accepted here that is not this bug. The
// marker member is the discovered form of the marker control, because the
// set cannot know the canary the tester used.
//
// It is a menu and not a checklist. Every obligation has to be established
// before a finding is proved, and a control whose question does not apply to
// the finding contradicts it, so a caller verifying an injection finding
// uses the marker, status, body and reflection controls and leaves the
// secret one out. The set is the starting point rather than the answer.
func DefaultControls() []Control {
	return []Control{
		MarkerGuardControl(),
		StatusControl(),
		BodyChangeControl(),
		ReflectionControl(),
		SecretControl(),
	}
}

// canaryValues returns the values the request carried that are long enough
// to be recognised when they come back.
//
// The order is fixed and the keys are sorted inside it, so the same exchange
// always yields the same list and a decision built from it never depends on
// map order.
func canaryValues(o Observation) []string {
	values := payloadValues(o)
	out := make([]string, 0, len(values))
	for _, value := range values {
		if len(value) >= minCanaryLength {
			out = append(out, value)
		}
	}
	return out
}

// payloadValues returns the values the request carried that could come back
// in the response: the query parameter values of the URL and the values of
// the request body.
func payloadValues(o Observation) []string {
	var out []string
	if parsed, err := url.Parse(strings.TrimSpace(o.URL)); err == nil {
		query := parsed.Query()
		for _, key := range sortedValueKeys(query) {
			for _, value := range query[key] {
				if value != "" {
					out = append(out, value)
				}
			}
		}
	}
	return append(out, bodyValues(o.RequestBody)...)
}

// bodyValues returns the values a request body carried: the string values of
// a JSON object, the values of a form body, or the body itself when it is
// neither.
func bodyValues(body string) []string {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "{") {
		if values, ok := jsonValues(trimmed); ok {
			return values
		}
	}
	if values, ok := formValues(trimmed); ok {
		return values
	}
	return []string{trimmed}
}

// jsonValues returns the string values of a JSON object, sorted by key.
func jsonValues(body string) ([]string, bool) {
	var fields map[string]any
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		return nil, false
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := fields[key].(string); ok && value != "" {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// formValues returns the values of a form body, sorted by key.
func formValues(body string) ([]string, bool) {
	values, err := url.ParseQuery(body)
	if err != nil {
		return nil, false
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		for _, value := range values[key] {
			if value != "" {
				out = append(out, value)
			}
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// stripAll removes every occurrence of every value from a text.
func stripAll(text string, values []string) string {
	out := text
	for _, value := range values {
		out = strings.ReplaceAll(out, value, "")
	}
	return out
}

// collapseSpace folds every run of whitespace into a single space and trims
// the ends, so a comparison of two bodies is not decided by the template's
// indentation.
func collapseSpace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// sortedKeys returns the keys of a header map in a fixed order.
func sortedKeys(headers map[string]string) []string {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// sortedValueKeys returns the keys of a query map in a fixed order.
func sortedValueKeys(values url.Values) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
