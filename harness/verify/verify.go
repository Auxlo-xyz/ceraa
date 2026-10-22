package verify

// verify.go is the policy itself: the four verdicts, the record of one
// exchange, the obligations a control enforces, and the rule that turns a
// list of obligations into a verdict.
//
// The rule is the whole package. Everything else is the machinery that feeds
// it, and the machinery is deliberately replaceable: a caller that needs a
// different question writes a different control, and the verdict rule does
// not move.

import (
	"fmt"
	"strings"
	"time"
)

// Verdict is the outcome of a verification.
//
// The vocabulary is four states and no more. PROVEN is the only reportable
// one, because a finding that is not proved is not a finding. The other
// three exist so that a failure to prove is not mistaken for a proof of
// absence: REFUTED says the evidence says no, UNPROVEN says there was
// nothing to check, and INCONCLUSIVE says the check could not run.
type Verdict string

const (
	// Proven means the exchange shows the bug.
	Proven Verdict = "PROVEN"
	// Refuted means the exchange shows the bug is not there.
	Refuted Verdict = "REFUTED"
	// Unproven means there was nothing to check.
	Unproven Verdict = "UNPROVEN"
	// Inconclusive means the check could not run.
	Inconclusive Verdict = "INCONCLUSIVE"
)

// Observation is one recorded exchange between the tester and the target.
//
// It is a record, not a live request: the package fetches nothing, and a
// caller that wants live exchanges has to make them and hand them over.
type Observation struct {
	// URL is what was requested.
	URL string
	// Method is the HTTP method.
	Method string
	// RequestBody is what was sent.
	RequestBody string
	// Status is the status code that came back.
	Status int
	// ResponseBody is what came back.
	ResponseBody string
	// ResponseHeaders are the headers that came back.
	ResponseHeaders map[string]string
	// Baseline is the response to the same request without the payload. It
	// is what a naive check would have accepted, and the marker guard
	// compares against it.
	Baseline *Observation
	// At is when the observation was made.
	At time.Time
}

// Obligation is the thing a control enforces.
type Obligation struct {
	// Name identifies the obligation in the report.
	Name string
	// Detail says what was enforced, in words a reader can disagree with.
	Detail string
	// State is how the obligation resolved.
	State ObligationState
	// Evidence is the bounded detail behind the state.
	Evidence string
}

// ObligationState is how an obligation resolved.
//
// Three states and no more. Established means the obligation was met.
// Contradicted means the observation says the opposite. Missing means nothing
// was observed that could answer it, which is a different claim from
// Contradicted: one is a result and the other is the absence of one.
type ObligationState string

const (
	// Established means the obligation was met.
	Established ObligationState = "Established"
	// Contradicted means the observation says the opposite.
	Contradicted ObligationState = "Contradicted"
	// Missing means nothing was observed that could answer it.
	Missing ObligationState = "Missing"
)

// Control is one check.
//
// The question is the point of the control: what would a naive check have
// accepted here that is not this bug? A 200 is what a public page returns. A
// reflected parameter is what a web server does. A control that cannot state
// its question cannot say what its result means.
type Control struct {
	// Name identifies the control.
	Name string
	// Question is what a naive check would have accepted that is not this bug.
	Question string
	// Enforce returns the obligation the observation resolves.
	Enforce func(o Observation) Obligation
}

// Decision is the result of a verification.
type Decision struct {
	// Verdict is what the exchange was worth.
	Verdict Verdict
	// Obligations are the resolved obligations, one per control.
	Obligations []Obligation
	// Reasons explain the verdict in words.
	Reasons []string
}

// Decide is the policy: pure, total, and the definition of what a finding
// is worth.
//
// Every input path returns a verdict. There is no error to handle and no
// input that refuses to answer, because a validator whose silence a reader
// cannot interpret is a validator whose failures look like results.
//
// The rule, in order:
//
//   - a contradicted obligation refutes the finding: the evidence says no
//   - every obligation established, and at least one of them, proves it
//   - an obligation that could not be resolved at all leaves the finding
//     inconclusive: the check could not run
//   - an obligation missing beside obligations that were established leaves
//     the finding unproven: there was something to check and it was not
//     enough
//
// The order is deliberate. A refutation is a claim and a missing observation
// is not, so one contradicted obligation outranks a dozen that could not run.
func Decide(o Observation, controls []Control) Decision {
	if len(controls) == 0 {
		return Decision{
			Verdict: Unproven,
			Reasons: []string{"no control was supplied, so there was nothing to check"},
		}
	}

	obligations := make([]Obligation, 0, len(controls))
	for _, control := range controls {
		obligations = append(obligations, enforce(control, o))
	}

	decision := Decision{Obligations: obligations}
	establishedCount, contradictedCount, missingCount := 0, 0, 0
	for _, obligation := range obligations {
		switch obligation.State {
		case Established:
			establishedCount++
		case Contradicted:
			contradictedCount++
		default:
			missingCount++
		}
	}

	switch {
	case contradictedCount > 0:
		decision.Verdict = Refuted
	case missingCount == 0 && establishedCount > 0:
		decision.Verdict = Proven
	case missingCount == len(obligations):
		// Nothing was answered at all. The check did not run, which is a
		// different thing from a check that ran and found nothing.
		decision.Verdict = Inconclusive
	default:
		decision.Verdict = Unproven
	}

	decision.Reasons = decisionReasons(decision.Verdict, obligations, establishedCount)
	return scrub(decision)
}

// enforce runs one control and turns whatever it does into an obligation.
//
// The recovery is the point. A control is a function a caller supplied, and a
// panic inside it must not take the process down. A control that panicked has
// not answered the question, so the obligation is Missing and the panic is
// named: a reader who cannot see why a check failed cannot use the result.
func enforce(control Control, o Observation) (obligation Obligation) {
	name := strings.TrimSpace(control.Name)
	if name == "" {
		name = "unnamed control"
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			obligation = Obligation{
				Name:     name,
				Detail:   "the control panicked, so the obligation could not be resolved",
				State:    Missing,
				Evidence: fmt.Sprintf("panic: %v", recovered),
			}
		}
	}()

	if control.Enforce == nil {
		return Obligation{
			Name:     name,
			Detail:   "the control carries no obligation function",
			State:    Missing,
			Evidence: "no obligation function was supplied",
		}
	}

	obligation = control.Enforce(o)
	obligation.Name = firstNonEmpty(obligation.Name, name)
	obligation.Detail = firstNonEmpty(obligation.Detail, control.Question)
	switch obligation.State {
	case Established, Contradicted:
	case Missing:
	default:
		// A state outside the vocabulary is not a claim, it is a typo, and
		// a typo has not answered the question either.
		obligation = Obligation{
			Name:     obligation.Name,
			Detail:   "the obligation carries a state outside the vocabulary",
			State:    Missing,
			Evidence: fmt.Sprintf("unknown obligation state %q", obligation.State),
		}
	}
	return obligation
}

// decisionReasons explains the verdict in words.
//
// Every obligation that did not resolve is named, with what was observed, so
// a reader can disagree with the result instead of having to take it.
func decisionReasons(verdict Verdict, obligations []Obligation, established int) []string {
	reasons := make([]string, 0, len(obligations)+1)
	switch verdict {
	case Proven:
		reasons = append(reasons, fmt.Sprintf("every obligation was established (%d of %d)", established, len(obligations)))
		return reasons
	case Refuted:
		reasons = append(reasons, "the evidence contradicts the finding")
	case Unproven:
		reasons = append(reasons, "the finding is not established: an obligation went unmet")
	case Inconclusive:
		reasons = append(reasons, "the check could not run: no obligation could be resolved")
	}
	for _, obligation := range obligations {
		if obligation.State == Established {
			continue
		}
		reasons = append(reasons, reasonFor(obligation))
	}
	return reasons
}

// reasonFor renders one obligation that did not resolve.
func reasonFor(obligation Obligation) string {
	what := strings.TrimSpace(obligation.Evidence)
	if what == "" {
		what = strings.TrimSpace(obligation.Detail)
	}
	if what == "" {
		return fmt.Sprintf("%s was %s", obligation.Name, obligation.State)
	}
	return fmt.Sprintf("%s was %s: %s", obligation.Name, obligation.State, what)
}

// scrub masks every credential-shaped value the decision carries.
//
// The secret control already masks what it finds. This is the net under it,
// because a decision that quotes a credential in full is the one place
// Ceraa's own redaction guarantee would be defeated.
func scrub(decision Decision) Decision {
	for i := range decision.Obligations {
		decision.Obligations[i].Name = MaskSecrets(decision.Obligations[i].Name)
		decision.Obligations[i].Detail = MaskSecrets(decision.Obligations[i].Detail)
		decision.Obligations[i].Evidence = MaskSecrets(decision.Obligations[i].Evidence)
	}
	for i := range decision.Reasons {
		decision.Reasons[i] = MaskSecrets(decision.Reasons[i])
	}
	return decision
}

// firstNonEmpty returns the first value that carries something.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// established is the obligation for a check that fired.
func established(evidence string) Obligation {
	return Obligation{State: Established, Evidence: evidence}
}

// contradicted is the obligation for a check that looked and did not fire.
func contradicted(evidence string) Obligation {
	return Obligation{State: Contradicted, Evidence: evidence}
}

// missing is the obligation for a check that had nothing to look at.
func missing(evidence string) Obligation {
	return Obligation{State: Missing, Evidence: evidence}
}
