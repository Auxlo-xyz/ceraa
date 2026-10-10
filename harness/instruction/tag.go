package instruction

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ScopeTagging is how a rule that declares no scope of its own gets one.
//
// It exists because tagging by hand does not survive contact with a growing file.
// A rule added today with no [applies=...] compiles unscoped, an unscoped rule is
// delivered once per run and then never again, and nothing says so: the compile is
// silent about it and the rule simply stops appearing. Measured on this repo's own
// AGENTS.md, 152 rules compiled with zero carrying a scope or a trigger, which
// makes the observer's path matching dead weight.
//
// Two passes, and the order is the whole design.
//
// The DETERMINISTIC pass needs no model and no network. A rule's own text usually
// names the thing it is about - "every network tool goes through harness/netguard"
// names a directory that exists. A token that resolves against the project root is
// a scope the file itself asserted, so it is accepted without asking anyone.
//
// The PROPOSED pass asks a model for the rest. A proposal is accepted only when it
// names a path that resolved at compile time; the filesystem confirms what the
// model suggested. That is the same discipline the evidence store describes - the
// index identifies candidates, the source of truth verifies.
//
// The rejected case is the part worth being explicit about, because it is the
// opposite of the safe default. A rule that asked for a scope the filesystem
// cannot confirm is WITHHELD from delivery and reported as a hard finding, not
// left unscoped and not demoted to a warning. Reasoning:
//
// Leaving it unscoped makes it indistinguishable from a rule that never wanted a
// scope, so the failure is silent and the rule quietly reverts to once-per-run.
// Warning and continuing is a softer version of the same silence: the log line is
// read by nobody at the moment it matters, and the rule still never fires again.
// A hard finding is the only one of the three that a person has to look at, and it
// is reported next to the line that caused it.
//
// Withholding rather than falling back is the deliberate cost. A rule whose path
// has genuinely been deleted - "never reintroduce harness/search", where the
// package is gone - stops being delivered, which is wrong for that rule and right
// for the policy: the file is asserting a scope the tree no longer supports, and
// the answer is to fix the file, not to have the harness quietly guess.

// pathTokenRe finds the path-shaped words a rule is likely to be about.
//
// It requires a separator and a minimum length so that URL fragments, dates,
// percentages and file extensions alone do not qualify. A token with no separator
// is a base name and matches too broadly to confirm anything.
var pathTokenRe = regexp.MustCompile(`[A-Za-z0-9_/][A-Za-z0-9_./-]*[A-Za-z0-9_./-]/[A-Za-z0-9_./-]*`)

// minPathTokenLen is the shortest token worth resolving.
const minPathTokenLen = 5

// ScopeProposal is a model's suggested scope for one rule.
type ScopeProposal struct {
	// RuleID names the rule this is for. A proposal for an unknown id is
	// discarded rather than applied, because the id is the only thing tying the
	// pair together and a stray one would silently mis-scope an unrelated rule.
	RuleID ID
	// Applies is a project-relative path or glob.
	Applies string
	// Triggers is the trigger kind list, in the same spelling the file uses.
	Triggers []string
	// Why is the model's reason, carried so a rejected proposal can be reported
	// with its rationale rather than as a bare disagreement.
	Why string
}

// Proposer suggests scopes for rules that declared none.
//
// A function rather than an interface so a caller can wire nil and get the
// deterministic pass alone, and so the implementation can be swapped without the
// package depending on a provider package. It mirrors memory.MakeLearner's shape:
// an offline, once-per-compile hook that is opt-in and whose failure skips the
// step rather than breaking the run.
type Proposer func(ctx context.Context, req ProposeRequest) ([]ScopeProposal, error)

// ProposeRequest is what a proposer is shown.
type ProposeRequest struct {
	// Source is the project-relative file, for the proposal's provenance.
	Source string
	// Rules are the untagged rules, with their text.
	Rules []Instruction
	// Candidates are the tokens seen in those rules that resolved against the
	// root. A proposer is shown the candidate set rather than the whole tree so
	// it cannot invent a path that was never named.
	Candidates []string
}

// resolveScope resolves a proposed Applies against the project root.
//
// A glob is resolved against its longest literal prefix, because asking the
// filesystem about "harness/netguard/**" answers about a path that does not exist
// as such, while "harness/netguard" does. That is the same trick the matcher
// already plays for relative patterns.
//
// Returns the confirmed scope as spelled, and whether it confirmed. An empty
// Applies confirms nothing: a proposal that asked for no scope is not a scope.
func resolveScope(root, applies string) (string, bool) {
	applies = strings.TrimSpace(applies)
	if applies == "" {
		return "", false
	}
	probe := literalPrefix(applies)
	if probe == "" {
		return "", false
	}
	if !filepath.IsAbs(probe) {
		probe = filepath.Join(root, filepath.FromSlash(probe))
	}
	info, err := os.Stat(probe)
	if err != nil || info == nil {
		return "", false
	}
	return applies, true
}

// literalPrefix returns the leading path segments of a pattern that contain no
// wildcard, which is the part that may be asked about on disk.
func literalPrefix(pattern string) string {
	segments := strings.Split(filepath.ToSlash(pattern), "/")
	var kept []string
	for _, segment := range segments {
		if strings.ContainsAny(segment, "*?[") {
			break
		}
		kept = append(kept, segment)
	}
	if len(kept) == 0 {
		return ""
	}
	if kept[0] == "" && len(kept) > 1 {
		// An absolute pattern: keep the root marker and the first real segment.
		return "/" + strings.Join(kept[1:], "/")
	}
	return strings.Join(kept, "/")
}

// pathTokens returns the path-shaped tokens in a rule's text, de-duplicated and
// in first-seen order.
//
// First-seen rather than sorted so that when several tokens resolve, the one the
// author led with wins - the sentence's subject is usually its first path.
func pathTokens(text string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, raw := range pathTokenRe.FindAllString(text, -1) {
		token := strings.Trim(raw, "`.,;:()[]\"'")
		if len(token) < minPathTokenLen {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}

// TagResult is what one tagging pass produced.
type TagResult struct {
	Snapshot Snapshot
	// Withheld names the rules that asked for a scope the tree could not
	// confirm, with the line and the proposal that failed.
	Withheld []ScopeWithheld
	// Proposed is how many rules the model was asked about, so a run can tell
	// "the model found nothing to tag" from "there was nothing to tag".
	Proposed int
}

// ScopeWithheld is one rule whose scope could not be confirmed.
type ScopeWithheld struct {
	RuleID ID
	Line   int
	// Applies is the scope that was proposed and refused.
	Applies string
	// Why is the proposer's reason, when there was one.
	Why string
	// Reason says which pass refused it, so a finding can be acted on without
	// re-running the tool.
	Reason string
}

// TagScopes runs the deterministic pass and then, when configured, the proposed
// pass, and returns the snapshot with every rule that could be scoped scoped.
//
// The deterministic pass sees a token the rule text itself named. That is the
// rule asserting its own scope through the words it uses, and it is the reason
// the model is not on the critical path for the rules that matter most.
//
// ctx is accepted because the proposer is a network call. It is deliberately not
// threaded through the deterministic pass, which does no I/O beyond a stat.
func TagScopes(ctx context.Context, root, source string, snap Snapshot, propose Proposer) TagResult {
	result := TagResult{Snapshot: snap}
	if root == "" {
		// No root means nothing can be confirmed. A rule that names no confirmable scope
		// is left global rather than withheld: withholding would strip every rule
		// from a caller with no root, which is a configuration state and not a
		// decision about those rules.
		return result
	}

	candidates := map[string]struct{}{}
	var pending []int
	for i, rule := range result.Snapshot.Instructions {
		if rule.Applies != "" || len(rule.Triggers) > 0 {
			continue
		}
		tokens := pathTokens(rule.Text)
		if len(tokens) == 0 {
			// A global rule, and this is the important distinction.
			//
			// "Never use em dashes", "always use Auxlo XYZ" - no path exists
			// because none applies. Forcing a scope on these, or withholding
			// them for not having one, would strip most of the document from
			// the agent: measured on this repo's own AGENTS.md, 135 of 152
			// rules name no path at all, and treating them as awaiting a scope
			// withholds 89% of the project's instructions.
			//
			// A global rule stays unscoped and is delivered once per run, which
			// is the correct schedule for a rule that is always true and never
			// more true than on the first turn.
			continue
		}
		var confirmed string
		for _, token := range tokens {
			candidates[token] = struct{}{}
			if _, ok := resolveScope(root, token); ok {
				confirmed = token
				break
			}
		}
		if confirmed == "" {
			pending = append(pending, i)
			continue
		}
		result.Snapshot.Instructions[i].Applies = confirmed
	}

	if len(pending) == 0 {
		return result
	}

	if propose != nil {
		ordered := make([]string, 0, len(candidates))
		for token := range candidates {
			ordered = append(ordered, token)
		}
		// Sorted so a proposer shown the same rules is shown the same candidate
		// list, which is what makes a run's behaviour attributable at all.
		sortStrings(ordered)

		untagged := make([]Instruction, 0, len(pending))
		for _, i := range pending {
			untagged = append(untagged, result.Snapshot.Instructions[i])
		}
		result.Proposed = len(untagged)

		proposals, err := propose(ctx, ProposeRequest{Source: source, Rules: untagged, Candidates: ordered})
		if err == nil {
			byID := map[ID]ScopeProposal{}
			for _, proposal := range proposals {
				byID[proposal.RuleID] = proposal
			}
			stillPending := pending[:0]
			for _, i := range pending {
				rule := result.Snapshot.Instructions[i]
				proposal, ok := byID[rule.ID]
				if !ok {
					continue
				}
				if confirmed, ok := resolveScope(root, proposal.Applies); ok {
					result.Snapshot.Instructions[i].Applies = confirmed
					result.Snapshot.Instructions[i].Triggers = proposal.Triggers
					continue
				}
				result.Withheld = append(result.Withheld, ScopeWithheld{
					RuleID:  rule.ID,
					Line:    rule.Line,
					Applies: proposal.Applies,
					Why:     proposal.Why,
					Reason:  "proposed scope does not resolve against the project",
				})
				result.Snapshot.Instructions[i].Applies = withheldApplies
			}
			pending = stillPending
		}
	}

	// Whatever is still untagged made no proposal anyone could confirm, which is
	// a different thing from a proposal that was refused.
	//
	// A rule that merely mentions something path-shaped is usually naming a
	// thing that is not a filesystem path at all: a Go import (ceraa/internal/...),
	// a module path, a shell variable (RUNTIME_DIR/run_ceraa.sh) or a
	// home-relative one (~/.config/ceraa). Measured on this repo's own AGENTS.md,
	// every rule in this category is one of those, and withholding them would
	// strip 25 legitimate rules for no stale path among them.
	//
	// So the strict branch applies to PROPOSALS, not to rules. A proposal the
	// filesystem refused is withheld, because somebody did claim a scope and the
	// claim did not hold. A rule that named nothing resolvable is global and has
	// always been global - it is left alone rather than punished for a pattern
	// false positive.
	for _, i := range pending {
		result.Snapshot.Instructions[i].Applies = ""
	}
	return result
}

// withheldApplies marks a rule as deliberately withheld.
//
// A non-empty sentinel rather than an empty string so the observer cannot fire
// it, and so the reason is visible in a dump without a second field. It names a
// directory that cannot exist, so no target can ever match it.
const withheldApplies = "<withheld: no confirmable scope>"

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
