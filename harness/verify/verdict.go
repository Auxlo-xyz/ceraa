// Package verify is Ceraa's answer to a question it currently cannot answer:
// when the agent says it found a bug, what stops the same agent from confirming
// it?
//
// Today it cannot. The model proposes a finding and the model reads it back, and
// because both halves share one transcript and one set of tools, agreement is
// cheap and the model's prior is that a bug was requested. That prior is the
// problem. XBOW reached the top of HackerOne's US leaderboard and concluded from
// their own runs that the models were good at finding and bad at validating,
// so they used "something other than an LLM" for the second half.
//
// So the spine here is deliberately dull. No model, no network, no judgement
// calls. The logic is a total function over recorded evidence, which is what
// makes it testable, and every path returns a verdict rather than falling
// through to a default.
//
// The idea is borrowed from awarexone/Agentic-Bug-Hunter, whose `decide()` is
// the reference. The one idea worth more than the rest is `markerInBaseline`:
// a validator that fetches a fresh response, finds its own canary in it, and
// declares success has proved that the server echoes input, which is almost
// always true and almost never a vulnerability. Requiring the marker to have
// been present in the ORIGINAL recorded response as well is what separates "my
// payload executed" from "my payload was reflected".
//
// This package is the spine and nothing else. It does not fetch, it does not
// probe, and it does not talk to anyone's server. Feeding it live exchanges
// later is a separate decision, and this package is shaped so that decision can
// be made without changing any of the logic below.
package verify

import "strings"

// Verdict is what a re-derivation concluded.
//
// The four values are the whole policy. The important property is that they are
// not a pass/fail pair with a nuance: UNPROVEN and INCONCLUSIVE suppress, and
// they suppress for different reasons, and neither of them is a claim that the
// finding is wrong.
type Verdict string

const (
	// Proven means there is an evidence bundle AND the confirmation marker was in
	// both the original recorded response and the fresh re-derivation. Only this
	// one is reportable.
	Proven Verdict = "PROVEN"
	// Refuted means the re-derivation ran cleanly and the marker was gone. The
	// finding is contradicted by evidence, which is a real answer.
	Refuted Verdict = "REFUTED"
	// Unproven means there was nothing to check, or the check cannot be trusted:
	// no bundle, or no marker to look for. It is not a claim of safety. "I could
	// not confirm this" is not "this is fine".
	Unproven Verdict = "UNPROVEN"
	// Inconclusive means the check could not run: unreachable, missing credential,
	// a mutating method that did not re-fire. The finding is held, not discarded
	// and not reported.
	//
	// This state is the one a validator is most tempted to collapse into Refuted,
	// and collapsing it is how a validator starts eating real findings silently.
	Inconclusive Verdict = "INCONCLUSIVE"
)

// Reportable reports whether a verdict may become an output.
//
// Only Proven. Deliberately not "Proven or Inconclusive": the whole value of
// this package is that it fails closed, and a gate that leaks in the uncertain
// case is a gate that leaks.
func (v Verdict) Reportable() bool { return v == Proven }

// Suppressed reports whether a verdict is withheld from the output. Both
// Unproven and Refuted are suppressed, and the distinction between them is
// carried by the reason rather than by whether anything is shown.
func (v Verdict) Suppressed() bool { return v != Proven }

// String makes a verdict printable without a cast at every call site.
func (v Verdict) String() string { return string(v) }

// Decision is a verdict plus the reason for it.
//
// The reason is not decoration. It is what a reader needs in order to disagree:
// "the marker was absent on re-derivation" and "there was no marker to look for"
// are the difference between a bug that was not there and a check that was not
// done, and a report that collapses them teaches nobody anything.
type Decision struct {
	Verdict Verdict `json:"verdict"`
	Reason  string  `json:"reason"`
}

// Reportable is Decision's answer to the only question a caller has.
func (d Decision) Reportable() bool { return d.Verdict.Reportable() }

// Rederivation is the observable result of independently re-running a finding.
//
// It is evidence about the check, not about the bug, which is why the fields are
// all about whether the check could speak rather than what it found.
type Rederivation struct {
	// Attempted is whether the check got as far as re-sending the request. An
	// un-attempted re-derivation cannot refute anything.
	Attempted bool `json:"attempted"`
	// Reachable is whether the endpoint answered at all.
	Reachable bool `json:"reachable"`
	// MarkerAvailable is whether there is any signal that could prove this
	// finding. Without one, a check has nothing to check.
	MarkerAvailable bool `json:"marker_available"`
	// MarkerInBaseline is whether the marker was present in the ORIGINAL
	// recorded response.
	//
	// This is the false-Proven guard, and it is the most important field in the
	// package. A marker that appears only in the fresh response proves the server
	// echoes input, which is the behaviour of essentially every web server on the
	// internet and is not a vulnerability.
	MarkerInBaseline bool `json:"marker_in_baseline"`
	// MarkerPresent is whether the marker is in the fresh response.
	MarkerPresent bool `json:"marker_present"`
	// Status is the HTTP status the re-derivation observed, when there was one.
	Status int `json:"status,omitempty"`
	// Detail carries the specific reason for a negative, when there is one.
	Detail string `json:"detail,omitempty"`
}

// Decide maps "is there anything to check" and "what did checking show" onto a
// verdict.
//
// It is pure and total: no I/O, no clock, no globals, every input path returns.
// That is the point. A policy you can read in one screen can be the definition
// rather than an implementation of the policy, and it can be tested exhaustively
// rather than by example.
//
// Order matters and is not incidental. The cheap disqualifiers come first so the
// reason reported is the most specific true one: "there is nothing to check" is
// a truer answer than "the marker was absent", and reporting the latter for the
// former is how an incomplete check starts reading as a clean result.
func Decide(hasBundle bool, rd Rederivation) Decision {
	if !hasBundle {
		return Decision{Unproven, "no evidence bundle — nothing to verify"}
	}
	if !rd.Attempted {
		return Decision{Inconclusive, firstNonEmpty(rd.Detail, "re-derivation was not attempted")}
	}
	if !rd.Reachable {
		return Decision{Inconclusive, firstNonEmpty(rd.Detail, "endpoint unreachable")}
	}
	if !rd.MarkerAvailable {
		// By policy this is Unproven and never a quiet pass. "We looked and saw
		// nothing" is a statement about the check, not about the target.
		return Decision{Unproven, "no confirmation marker — cannot independently prove; supply one"}
	}
	if !rd.MarkerInBaseline {
		// The marker is absent from the original response, so its presence now
		// cannot be attributed to this finding. It could be a server banner, or
		// attacker input reflected onto an error page.
		return Decision{Unproven, "marker was absent from the original recorded response — its presence on re-derivation does not prove this finding"}
	}
	if rd.MarkerPresent {
		return Decision{Proven, detailOr("marker present in both the original and the fresh response", rd)}
	}
	return Decision{Refuted, detailOr("marker absent on re-derivation", rd)}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func detailOr(fallback string, rd Rederivation) string {
	if trimmed := strings.TrimSpace(rd.Detail); trimmed != "" {
		return trimmed
	}
	return fallback
}
