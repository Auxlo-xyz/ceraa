package verify

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// A control request is the answer to one question: what would a naive check
// have accepted here that is not actually this bug?
//
// Every class has one, and the reason they matter is that the naive check is
// almost always satisfied by ordinary behaviour. A 200 is what a public page
// returns. A reflected parameter is what a web server does. A version banner is
// what every host sends. Without a control, a validator reports the server's
// normal behaviour back to the operator as a vulnerability.
//
// Each control here is a pure predicate over a recorded Bundle. None of them
// opens a socket, which is deliberate: the control is the part worth getting
// right, and it should be settled before anyone decides what is allowed to make
// the request that feeds it.

// Control names a kind of proof obligation.
type Control string

const (
	// ControlCrossIdentity is for anything claiming to reach another principal's
	// data. The most common way this is faked is reading your own data and calling
	// it a breach, which is why the obligation is two identities plus a denial.
	ControlCrossIdentity Control = "cross-identity"
	// ControlExecution is for anything claiming code ran. Reflection is not
	// execution.
	ControlExecution Control = "execution"
	// ControlDifferential is for anything claiming a payload changed behaviour.
	// The signal has to appear on the payload AND be absent from a baseline.
	ControlDifferential Control = "differential"
	// ControlRedirect is for anything claiming an attacker-chosen destination.
	// A reflected parameter that never becomes a Location is not a redirect.
	ControlRedirect Control = "redirect"
	// ControlContent is for anything claiming a secret was disclosed. A reachable
	// path is not a disclosure; the body has to carry something.
	ControlContent Control = "content"
	// ControlCallback is for anything blind. Nothing in-band can confirm it, so
	// the obligation is an out-of-band interaction that was actually observed.
	ControlCallback Control = "callback"
)

// Request is what a caller asserts about a finding, in the terms the control
// needs. Every field is evidence, not a conclusion: a caller filling these in is
// still only making a claim, and the control decides what the claim is worth.
type Request struct {
	// Control is which obligation applies.
	Control Control
	// Marker is the unique string planted in the payload. It is what proves this
	// finding rather than a generic behaviour.
	Marker string
	// AttackerLabel names the exchange made with the attacker's identity.
	AttackerLabel string
	// AnonymousLabel names the exchange made with no identity at all.
	AnonymousLabel string
	// BaselineLabel names the exchange made with a benign value.
	BaselineLabel string
	// PayloadLabel names the exchange made with the payload.
	PayloadLabel string
	// AttackerIdentity and VictimMarker describe the cross-identity claim.
	AttackerIdentity string
	VictimMarker     string
	// ExecutionChannel is the observation kind that would prove execution, for
	// example "exec:dialog".
	ExecutionChannel string
	// Signal is the text whose presence proves a behaviour change.
	Signal string
	// ExpectedHost is the destination an attacker would control.
	ExpectedHost string
	// ContentPatterns are the regular expressions whose match proves disclosure.
	ContentPatterns []string
}

// ControlResult is the outcome of a control.
type ControlResult struct {
	// Kind is the folded outcome of every obligation.
	Kind OutcomeKind `json:"kind"`
	// Satisfied is whether the obligation was met. It is Kind == Established, kept
	// because it is the question almost every caller actually asks.
	Satisfied bool `json:"satisfied"`
	// Obligations lists what the control required and whether each was met. It is
	// reported whole on success too, because "proven" with no shown work is
	// indistinguishable from an unproven claim that asserted itself.
	Obligations []Obligation `json:"obligations"`
	// Missing is the one unmet requirement, phrased as what would satisfy it. It
	// is set only when the outcome is Missing, because "collect this" and "the
	// evidence says no" are different instructions.
	Missing string `json:"missing,omitempty"`
	// Summary carries the contradiction when the outcome is Contradicted.
	Summary string `json:"summary,omitempty"`
}

// OutcomeKind is how a control requirement resolved.
//
// The three values are the point. A boolean cannot tell "the evidence says this
// is not a bug" from "the evidence needed to say anything was never collected",
// and that difference is the difference between refuting a finding and losing
// one. Reporting both as "failed" is how a validator silently discards real
// bugs while its logs show rejections.
type OutcomeKind string

const (
	// Established means the obligation was met.
	Established OutcomeKind = "established"
	// Contradicted means the evidence was collected and it says no.
	Contradicted OutcomeKind = "contradicted"
	// Missing means the evidence needed was never collected, so nothing was said.
	Missing OutcomeKind = "missing"
)

// Obligation is one requirement and its outcome.
type Obligation struct {
	Name   string      `json:"name"`
	Kind   OutcomeKind `json:"kind"`
	Detail string      `json:"detail,omitempty"`
}

// Met reports whether the obligation was satisfied. It reads better at call
// sites than comparing kinds, and it cannot drift from the kind it wraps.
func (o Obligation) Met() bool { return o.Kind == Established }

// result folds the obligations into one outcome.
//
// Precedence is deliberate. A contradiction outranks a missing requirement,
// because once the evidence says the finding is wrong, collecting the rest would
// not change that. A missing requirement outranks nothing, because it is not a
// statement about the target at all.
func result(obligations []Obligation) ControlResult {
	firstMissing := ""
	for _, obligation := range obligations {
		switch obligation.Kind {
		case Contradicted:
			return ControlResult{Kind: Contradicted, Obligations: obligations,
				Summary: obligation.Name + ": " + obligation.Detail}
		case Missing:
			if firstMissing == "" {
				firstMissing = obligation.Name + ": " + obligation.Detail
			}
		}
	}
	if firstMissing != "" {
		return ControlResult{Kind: Missing, Obligations: obligations, Missing: firstMissing}
	}
	return ControlResult{Kind: Established, Satisfied: true, Obligations: obligations}
}

// Evaluate checks one bundle against a control.
//
// It returns a ControlResult, not a Decision. The two are separate on purpose:
// the control says whether the proof obligation was met, and Decide says what
// that means for the finding. Keeping them apart means a caller can record the
// control's workings even when the outcome is "held", which is the case that
// needs explaining.
func Evaluate(b Bundle, req Request) ControlResult {
	switch req.Control {
	case ControlCrossIdentity:
		return crossIdentity(b, req)
	case ControlExecution:
		return execution(b, req)
	case ControlDifferential:
		return differential(b, req)
	case ControlRedirect:
		return redirect(b, req)
	case ControlContent:
		return content(b, req)
	case ControlCallback:
		return callback(b, req)
	}
	return result([]Obligation{{Name: "control is named", Kind: Missing, Detail: fmt.Sprintf("unknown control %q", req.Control)}})
}

// crossIdentity requires an attacker identity that saw the victim's marker AND
// an anonymous request for the same object that was denied.
//
// The second half is the whole control. Without it, "attacker got a 200 with
// this data" is equally consistent with the object simply being public, which is
// a different finding at a different severity.
func crossIdentity(b Bundle, req Request) ControlResult {
	var obligations []Obligation

	attacker, ok := b.Role(req.AttackerLabel)
	switch {
	case !ok:
		obligations = append(obligations, Obligation{
			Name: "a request made with the attacker's identity is recorded", Kind: Missing,
			Detail: "record the exchange under label " + labelOr(req.AttackerLabel, "attacker"),
		})
	case !strings.Contains(attacker.Body, req.VictimMarker):
		obligations = append(obligations, Obligation{
			Name: "the attacker's response carries the victim's marker", Kind: Contradicted,
			Detail: fmt.Sprintf("the attacker's own response does not contain %q, so no cross-tenant read was observed", req.VictimMarker),
		})
	default:
		obligations = append(obligations, Obligation{
			Name: "the attacker's response carries the victim's marker", Kind: Established,
			Detail: fmt.Sprintf("status %d, marker %q present", attacker.Status, req.VictimMarker),
		})
	}

	anonymous, ok := b.Role(req.AnonymousLabel)
	switch {
	case !ok:
		obligations = append(obligations, Obligation{
			Name: "an anonymous request for the same object is recorded", Kind: Missing,
			Detail: "record the exchange under label " + labelOr(req.AnonymousLabel, "anon") + "; without it a public object cannot be ruled out",
		})
	case anonymous.Status == 200 && strings.Contains(anonymous.Body, req.VictimMarker):
		obligations = append(obligations, Obligation{
			Name: "the anonymous control is denied", Kind: Contradicted,
			Detail: "anonymous access returned the same data, so the object is public and this is not an access-control break",
		})
	default:
		obligations = append(obligations, Obligation{
			Name: "the anonymous control is denied", Kind: Established,
			Detail: fmt.Sprintf("anonymous request returned %d without the marker", anonymous.Status),
		})
	}

	if strings.TrimSpace(req.AttackerIdentity) == "" {
		obligations = append(obligations, Obligation{
			Name: "the attacking identity is named", Kind: Missing,
			Detail: "record which account or session was used, so the claim is reproducible",
		})
	} else {
		obligations = append(obligations, Obligation{
			Name: "the attacking identity is named", Kind: Established, Detail: req.AttackerIdentity,
		})
	}

	return result(obligations)
}

// execution requires the marker to have executed on the original AND on the
// re-derivation, in a declared channel, and never merely to be reflected.
//
// The reflected case is listed as a failing obligation rather than being
// ignored, because "it came back in the HTML" is what an unverified XSS claim
// looks like and naming it is more useful than a bare refusal.
func execution(b Bundle, req Request) ControlResult {
	channel := req.ExecutionChannel
	if strings.TrimSpace(channel) == "" {
		channel = "exec"
	}
	var obligations []Obligation

	if strings.TrimSpace(req.Marker) == "" {
		obligations = append(obligations, Obligation{Name: "a unique marker is planted in the payload", Kind: Missing, Detail: "supply the marker string"})
	} else {
		obligations = append(obligations, Obligation{Name: "a unique marker is planted in the payload", Kind: Established, Detail: req.Marker})
	}

	obligations = append(obligations, executionObligation(
		"the payload executed in the original run", b.Original, channel, req.Marker, true))

	if !b.FreshAttempted {
		obligations = append(obligations, Obligation{
			Name: "the payload was re-run and executed again", Kind: Missing, Detail: "no re-derivation was attempted",
		})
	} else {
		obligations = append(obligations, executionObligation(
			"the payload was re-run and executed again", b.Fresh, channel, req.Marker, true))
	}

	return result(obligations)
}

// executionObligation separates "I looked and it did not fire" from "nothing was
// instrumented". An exchange carrying no observations at all has not answered the
// question; an exchange carrying observations of other kinds has answered it.
func executionObligation(name string, exchange Exchange, channel, marker string, isFresh bool) Obligation {
	if exchange.Observed(channel) {
		return Obligation{Name: name, Kind: Established, Detail: "observed " + channel}
	}
	if len(exchange.Observations) == 0 {
		return Obligation{Name: name, Kind: Missing, Detail: "no observations were recorded at all, so execution was never tested"}
	}
	detail := "observations were recorded but none on " + channel
	if marker != "" && MarkerIn(exchange, marker) && !exchange.Observed("exec") {
		detail = "the marker appears in the response body, which is reflection; no " + channel + " observation was recorded"
	}
	return Obligation{Name: name, Kind: Contradicted, Detail: detail}
}

// differential requires the signal on the payload and its absence from a benign
// baseline.
//
// One direction is not enough. A signature that appears on both responses is a
// banner, not a behaviour change, and a signature that appears on neither means
// the check never ran.
func differential(b Bundle, req Request) ControlResult {
	var obligations []Obligation
	signal := req.Signal
	if strings.TrimSpace(signal) == "" {
		signal = req.Marker
	}

	payload, ok := b.Role(req.PayloadLabel)
	switch {
	case !ok:
		obligations = append(obligations, Obligation{Name: "the payload response is recorded", Kind: Missing, Detail: "record it under label " + labelOr(req.PayloadLabel, "payload")})
	case strings.Contains(payload.Body, signal):
		obligations = append(obligations, Obligation{Name: "the signal appears on the payload", Kind: Established, Detail: fmt.Sprintf("status %d", payload.Status)})
	default:
		obligations = append(obligations, Obligation{Name: "the signal appears on the payload", Kind: Contradicted, Detail: fmt.Sprintf("%q not found in the payload response", signal)})
	}

	baseline, ok := b.Role(req.BaselineLabel)
	switch {
	case !ok:
		obligations = append(obligations, Obligation{Name: "the signal is absent from a benign baseline", Kind: Missing, Detail: "record a request with a benign value under label " + labelOr(req.BaselineLabel, "baseline")})
	case strings.Contains(baseline.Body, signal):
		obligations = append(obligations, Obligation{Name: "the signal is absent from a benign baseline", Kind: Contradicted, Detail: fmt.Sprintf("%q appears on the benign value too, so it is not caused by the payload", signal)})
	default:
		obligations = append(obligations, Obligation{Name: "the signal is absent from a benign baseline", Kind: Established, Detail: fmt.Sprintf("status %d, signal absent", baseline.Status)})
	}

	return result(obligations)
}

// redirect requires a 3xx whose Location actually resolves to the host the
// attacker chose. A reflected parameter is not a redirect, and this is the most
// common open-redirect false positive by a wide margin.
func redirect(b Bundle, req Request) ControlResult {
	var obligations []Obligation
	expected := strings.ToLower(strings.TrimSpace(req.ExpectedHost))
	if expected == "" {
		return result([]Obligation{{Name: "the attacker's host is named", Kind: Missing, Detail: "supply ExpectedHost"}})
	}
	obligations = append(obligations, Obligation{Name: "the attacker's host is named", Kind: Established, Detail: expected})

	ex := b.Fresh
	if !b.FreshAttempted {
		ex = b.Original
	}
	switch {
	case ex.Status == 0:
		obligations = append(obligations, Obligation{Name: "the response is a redirect", Kind: Missing, Detail: "no response was recorded"})
	case ex.Status < 300 || ex.Status > 399:
		obligations = append(obligations, Obligation{Name: "the response is a redirect", Kind: Contradicted, Detail: fmt.Sprintf("status %d is not 3xx", ex.Status)})
	case strings.TrimSpace(ex.HeaderValue("Location")) == "":
		obligations = append(obligations, Obligation{Name: "the response is a redirect", Kind: Contradicted, Detail: "3xx with no Location header"})
	default:
		obligations = append(obligations, Obligation{Name: "the response is a redirect", Kind: Established, Detail: fmt.Sprintf("status %d", ex.Status)})

		location := ex.HeaderValue("Location")
		resolved, err := url.Parse(location)
		host := ""
		if err == nil {
			if resolved.Host != "" {
				host = strings.ToLower(resolved.Hostname())
			} else if base, baseErr := url.Parse(ex.URL); baseErr == nil {
				if joined, joinErr := base.Parse(location); joinErr == nil {
					host = strings.ToLower(joined.Hostname())
				}
			}
		}
		if host == expected || (host != "" && strings.HasSuffix(host, "."+expected)) {
			obligations = append(obligations, Obligation{Name: "the Location resolves to the attacker's host", Kind: Established, Detail: host})
		} else {
			detail := fmt.Sprintf("Location resolved to %q, not %q", host, expected)
			if host == "" {
				detail = fmt.Sprintf("Location %q could not be resolved to a host", location)
			}
			obligations = append(obligations, Obligation{Name: "the Location resolves to the attacker's host", Kind: Contradicted, Detail: detail})
		}
	}

	return result(obligations)
}

// content requires the body to carry something that is actually a secret. A 200
// that serves an application shell is a reachable path, not a disclosure.
func content(b Bundle, req Request) ControlResult {
	var obligations []Obligation
	patterns := req.ContentPatterns
	if len(patterns) == 0 {
		patterns = DefaultSecretPatterns()
	}
	ex := b.Fresh
	if !b.FreshAttempted {
		ex = b.Original
	}

	switch {
	case ex.Status == 0:
		obligations = append(obligations, Obligation{Name: "the response is 200", Kind: Missing, Detail: "no response was recorded"})
	case ex.Status != 200:
		obligations = append(obligations, Obligation{Name: "the response is 200", Kind: Contradicted, Detail: fmt.Sprintf("status %d", ex.Status)})
	default:
		obligations = append(obligations, Obligation{Name: "the response is 200", Kind: Established, Detail: "200"})
	}
	for _, pattern := range patterns {
		expression, err := regexp.Compile(pattern)
		if err != nil {
			obligations = append(obligations, Obligation{Name: "the disclosure patterns compile", Kind: Missing, Detail: fmt.Sprintf("%q: %v", pattern, err)})
			continue
		}
		match := expression.FindString(ex.Body)
		if match == "" {
			continue
		}
		obligations = append(obligations, Obligation{Name: "the body carries a secret", Kind: Established, Detail: mask(match)})
		return result(obligations)
	}
	obligations = append(obligations, Obligation{Name: "the body carries a secret", Kind: Contradicted, Detail: "no secret pattern matched; a reachable path is not a disclosure"})
	return result(obligations)
}

// callback requires an observed out-of-band interaction carrying the marker.
// Nothing in-band can confirm a blind bug, and "we injected it and nothing came
// back" is never a confirmation.
func callback(b Bundle, req Request) ControlResult {
	var obligations []Obligation
	if strings.TrimSpace(req.Marker) == "" {
		obligations = append(obligations, Obligation{Name: "a marker is planted in the payload", Kind: Missing, Detail: "supply the marker"})
	} else {
		obligations = append(obligations, Obligation{Name: "a marker is planted in the payload", Kind: Established, Detail: req.Marker})
	}
	for _, side := range []struct {
		role     string
		exchange Exchange
		ran      bool
	}{{"original", b.Original, true}, {"re-derivation", b.Fresh, b.FreshAttempted}} {
		name := side.role + " received a callback carrying the marker"
		if !side.ran {
			obligations = append(obligations, Obligation{Name: name, Kind: Missing, Detail: "this run never happened, so no callback could arrive"})
			continue
		}
		found := false
		for _, observation := range side.exchange.Observations {
			if strings.HasPrefix(observation.Kind, "oob") && strings.Contains(observation.Detail, req.Marker) {
				found = true
				obligations = append(obligations, Obligation{Name: name, Kind: Established, Detail: observation.Kind})
				break
			}
		}
		if found {
			continue
		}
		if len(side.exchange.Observations) == 0 {
			obligations = append(obligations, Obligation{Name: name, Kind: Missing, Detail: "no interaction was observed at all; a blind bug cannot be confirmed in-band"})
			continue
		}
		obligations = append(obligations, Obligation{Name: name, Kind: Contradicted, Detail: "interactions were observed but none carried the planted marker"})
	}
	return result(obligations)
}

// Verify is the whole policy in one call.
//
// The mapping is not a lookup table, because the control has already done the
// thinking:
//
//   - Established is Proven, once the spine's one remaining demand is met. That
//     demand is that the marker was present in the ORIGINAL exchange. A control
//     can pass on evidence gathered entirely in the re-derivation, and an
//     unanchored marker is exactly the reflection-vs-execution confusion the
//     control exists to prevent. Two gates catching different mistakes is not
//     redundancy for its own sake.
//   - Contradicted is Refuted. The evidence was collected and it says no.
//   - Missing is Inconclusive. Nothing was said, so nothing is claimed.
//
// A pure "did the control pass" boolean would collapse the last two into one and
// turn every unrun check into a rejection.
func Verify(b Bundle, req Request) Decision {
	decision, _ := VerifyDetailed(b, req)
	return decision
}

// VerifyDetailed runs Verify and also returns the control's workings, so a
// Proven verdict carries its evidence and a held or refuted one carries the
// specific obligation that decided it.
func VerifyDetailed(b Bundle, req Request) (Decision, ControlResult) {
	control := Evaluate(b, req)
	switch control.Kind {
	case Established:
		if !MarkerIn(b.Original, req.Marker) {
			// The control passed, but on what? If the marker only ever appeared in
			// the re-derivation then the proof has no anchor in the original
			// observation, and an unanchored marker proves that the server echoes
			// input.
			return Decision{Unproven, "the control was satisfied but the marker was absent from the original recorded exchange, so the proof has no anchor"}, control
		}
		return Decision{Proven, "control satisfied and the marker is present in the original exchange"}, control
	case Contradicted:
		return Decision{Refuted, "control contradicted the claim: " + control.Summary}, control
	default:
		return Decision{Inconclusive, "control could not speak: " + control.Missing}, control
	}
}

func labelOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// mask shortens a matched secret so a report can quote what was found without
// republishing it. Ceraa redacts its own outputs, so a finding that quotes a
// credential in full is the one place that guarantee would be defeated.
func mask(match string) string {
	runes := []rune(match)
	if len(runes) <= 8 {
		return string(runes[:1]) + "…"
	}
	return string(runes[:6]) + "…" + string(runes[len(runes)-2:])
}

// DefaultSecretPatterns are the high-signal credential shapes.
//
// These deliberately mirror harness/redact rather than inventing a second set.
// The redactor already knows what a credential looks like because it exists to
// remove them; a scanner that disagrees with the redactor about that is a bug
// waiting to be found, and one list cannot drift from itself.
func DefaultSecretPatterns() []string {
	return []string{
		`AKIA[0-9A-Z]{16}`,
		`(?i)(aws_secret_access_key|aws_access_key_id|db_password|jwt_signing_key|secret_key|signing_key|private_key|client_secret|api[_-]?key|password|passwd|token)\s*[:=]\s*\S{4,}`,
		`(?i)(postgres|postgresql|mysql|mongodb(\+srv)?|redis|amqp)://[^\s"'<>]+:[^\s"'<>]+@`,
		`-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----`,
		`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`,
		`(?i)xox[abposr]-[A-Za-z0-9-]{10,}`,
		`(?i)gh[pousr]_[A-Za-z0-9]{20,}`,
		`(?i)github_pat_[A-Za-z0-9_]{20,}`,
		`(?i)sk-(?:ant|proj|or|live)-[A-Za-z0-9_-]{16,}`,
		`AIza[0-9A-Za-z_-]{30,}`,
	}
}
