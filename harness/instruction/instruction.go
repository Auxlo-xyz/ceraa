// Package instruction compiles project instruction files (AGENTS.md and
// anything else written in the same shape) into addressable rules, and delivers
// the relevant ones next to the operation they concern.
//
// # Why this exists
//
// Ceraa's system prompt used to carry a <memory> block re-read from disk on
// every request. That block sits before the provider's cache seam, so an edit to
// a memory file - or anything else volatile in it - invalidates the whole
// prefix and the request is re-processed from zero. A project instruction file
// re-read on every request has the same shape and the same cost, and it is
// worse: it is large, it changes rarely, and it is exactly the material a
// prompt cache is designed to hold still.
//
// So the rules move out of the permanent prompt and into the tool-result
// channel, where they are appended to the output of the operation they bear on.
// The prompt prefix stops moving; the instructions stop being paid for on
// turns that do not concern them.
//
// # This enforces nothing. Say it plainly.
//
// A text block cannot refuse a write, cannot block a network dial and cannot
// fail a build. `mandatory` buys exactly three things and no more:
//
//  1. a mandatory rule is never dropped in favour of a lower-ranked one, so it
//     survives a byte budget that an advisory rule would not,
//  2. it is delivered on every operation that matches it rather than once per
//     run like the rest, and
//  3. it is exempt from the run's delivered set, which is the mechanism behind
//     (2) and nothing else.
//
// That is the entire content of the word. It is not a sandbox, it is not a
// capability check, and a rule that says "never write to the source tree" is
// still only a sentence the model reads. Most of the rules in this repository's
// own AGENTS.md are enforced in code - the pid-slot guard, the removal of
// harness/search, netguard on every egress tool - and for those this compiler's
// only job is to surface the rule at the moment the operation is about to
// violate it, and to say nothing at all about preventing it. Where a rule
// deserves enforcement, it belongs in code where it can refuse; this package is
// the reminder, not the guard.
//
// Two bounds still apply to a mandatory rule and both are stated where they
// happen: the count limit in Select, and the rendered note's own ceiling in
// Annotate. A file carrying more mandatory rules than fit in one note delivers
// the top of them every time and says how many it did not show.
//
// # The compiler is pure, and that is the point.
//
// Compile takes a string and bytes and returns a value and diagnostics. No
// clock, no filesystem, no globals, every input path returns. That is the
// reason: a policy you can read in one screen can be the definition of the
// policy rather than an implementation of it, and it can be tested exhaustively
// instead of by example. Everything impure about this package lives in Store.
//
// There is deliberately no timestamp and no score on Instruction. A score is a
// function of a clock, and a clock in this package is the exact volatility being
// removed - a rule whose rank changed with the wall clock would re-rank the
// delivery on a turn where nothing about the work changed, which is the failure
// this package exists to avoid. Ordering is priority (a property of the text)
// and then document order (a property of the file). Both are stable across
// recompiles, both are visible in a diff, and neither needs a time source.
package instruction

import (
	"fmt"
	"strings"
)

// ID is a compiled rule's identity, and it is derived from the rule's own words.
//
// It is typed rather than a bare string because an id crosses package
// boundaries here - it is written into a per-run delivered set, compared against
// a stored set, and carried in a rendered block - and a mistake in any of those
// is silent. The type makes the compiler ask.
type ID string

// Priority is how strongly a rule is meant to bind, and it decides two things
// only: the order rules are offered in, and which of them a byte budget is
// allowed to drop.
type Priority string

const (
	// Mandatory is a standing prohibition or requirement the author wants
	// delivered on every matching operation.
	//
	// It is the strongest word available here and, per the package doc, it buys
	// delivery and ordering and nothing else.
	Mandatory Priority = "mandatory"
	// Advisory is a rule that should shape behaviour when it is relevant but does
	// not claim to be inviolable. It is the default for a bullet.
	Advisory Priority = "advisory"
	// Explanatory is context: the reasoning behind a rule, a description of what
	// a thing is. It is the default for a paragraph, because in a document like
	// AGENTS.md a bullet is nearly always a rule and a paragraph is nearly
	// always the reason for one.
	Explanatory Priority = "explanatory"
)

// rank orders priorities strongest first. Lower is stronger.
//
// It is a total order with no ties, so the delivery order of any two distinct
// rules is decided, and Annotate can drop from the end without a tiebreak that
// depends on map iteration order.
func (p Priority) rank() int {
	switch p {
	case Mandatory:
		return 0
	case Advisory:
		return 1
	default:
		return 2
	}
}

// Instruction is one compiled rule: a sentence lifted out of a markdown file,
// stripped of its metadata, and given an identity that does not move when the
// file around it is edited.
type Instruction struct {
	// ID identifies the rule by its own words, so an id recorded in a delivered
	// set yesterday still refers to the same sentence today after the file has
	// been reflowed, re-sectioned or reordered.
	//
	// It is not derived from the section path and an ordinal, which was
	// rejected: renaming a heading would renumber every rule beneath it, and an
	// id recorded yesterday would resolve silently to a different rule today.
	// That failure is invisible - nothing errors, the set simply suppresses the
	// wrong sentence forever - which is the worst shape a stale id can have.
	ID ID

	// Text is the rule as delivered: metadata tags stripped, whitespace
	// collapsed, no bullet marker, no heading path.
	//
	// Collapsed to one line because this text is appended to a tool result
	// between the result and the model's next turn, where a hanging indent costs
	// bytes on every subsequent request in the session and reads as a code block.
	Text string

	// Source is the project-relative path this rule came from, kept for the
	// rendered attribution and for a human following a rule back to the file.
	// Project-relative rather than absolute because an absolute path from one
	// checkout is noise in another, and nothing here resolves it for I/O.
	Source string

	// Line and EndLine are 1-based and inclusive, and are for humans only.
	//
	// Nothing selects, orders, de-duplicates or hashes on them. They exist so a
	// reader who disagrees with a delivered rule can open the file at it, and so
	// a diagnostic can point at the line that caused it.
	Line    int
	EndLine int

	// Section is the heading path this rule sits under, joined with " > ".
	//
	// It is context for the rendered attribution and for a human reading the
	// block. It is deliberately not part of ID or Hash: the heading is scope,
	// not wording, and two identical sentences under different headings are the
	// same rule as far as identity goes.
	Section string

	// Applies is a path glob narrowing where this rule is delivered, in exactly
	// the grammar memory.MatchesScope implements.
	//
	// Empty means the rule is general and matches every operation. The grammar
	// is enforced by the host's matcher rather than by this package, which is
	// why Matcher exists in select.go: glob semantics are the one thing here
	// that must not be a second implementation.
	Applies string

	// Triggers are additional conditions on delivery, each of which must hold.
	//
	// A bare word is a named condition ("error" fires only on a failed
	// operation). A tool restriction is recorded here prefixed with "tool:" -
	// "[tools=write,patch]" becomes "tool:write", "tool:patch" - because a tool
	// name is a trigger and not a scope, and folding it into Applies would make
	// the two grammars meet in one field where neither is explained.
	Triggers []string

	// Priority decides delivery order and what a byte budget may drop.
	Priority Priority

	// Hash is the full hex sha256 of Text. ID is a truncation of this same digest,
	// so the two cannot describe different rules.
	Hash string

	// Oversized records that Text was cut to fit MaxInstructionBytes.
	//
	// It is carried on the rule rather than left in a diagnostic alone because
	// the reader of a delivered rule is the one who needs to know it is partial.
	//
	// The digest is taken over the truncated text, not over the rule before the
	// cap, so that one sentence describes what this rule is: a rule whose Hash
	// did not describe its Text would be two facts about one sentence, and the
	// disagreement between them would be undetectable. The price is stated
	// rather than hidden: lowering MaxInstructionBytes re-identifies every rule
	// that was over it, so the cap is a constant and not a knob.
	Oversized bool

	// DuplicateOf names the rule whose words this one repeats, when this rule is
	// a later copy of identical text in the same file.
	//
	// Both copies are kept. A repeated sentence in a document is a fact about
	// the document, and silently collapsing it would make the compiled rule
	// count a lie.
	DuplicateOf ID
}

// Snapshot is one compile of one file, and is immutable once published.
//
// It is a value rather than a pointer because the store hands the same snapshot
// to concurrent readers while a recompile may be in flight, and a reader that
// has to take a lock to learn what is in the snapshot is a reader that blocks
// behind someone else's file read.
type Snapshot struct {
	// Source is the project-relative path this snapshot was compiled from.
	Source string
	// Digest is the hex sha256 of the exact bytes compiled, which is what lets a
	// store tell "the file changed" from "the file's mtime moved".
	Digest string
	// Instructions is in document order.
	Instructions []Instruction
}

// Len is how many rules this snapshot holds.
func (s Snapshot) Len() int { return len(s.Instructions) }

// Lookup finds a rule by id, reporting whether it is present.
func (s Snapshot) Lookup(id ID) (Instruction, bool) {
	for _, item := range s.Instructions {
		if item.ID == id {
			return item, true
		}
	}
	return Instruction{}, false
}

// DiagnosticKind classifies a compiler finding.
type DiagnosticKind string

const (
	// DiagUnclosedFence is a code fence that never closed, so the tail of the
	// file was consumed as code and produced no rules.
	DiagUnclosedFence DiagnosticKind = "unclosed-fence"
	// DiagUnterminatedTable is a run of table rows with no delimiter row, so the
	// rows were reference data that could not be identified as reference data
	// and were dropped rather than delivered as sentences.
	DiagUnterminatedTable DiagnosticKind = "unterminated-table"
	// DiagUnclosedComment is an HTML comment that never closed, so the tail was
	// consumed as comment text.
	DiagUnclosedComment DiagnosticKind = "unclosed-comment"
	// DiagDuplicate is repeated identical text within one file.
	DiagDuplicate DiagnosticKind = "duplicate"
	// DiagBroadScope is an Applies value with no separator and no dot, which is
	// the shape that matches a base name anywhere in the tree.
	DiagBroadScope DiagnosticKind = "broad-scope"
	// DiagUnresolvedScope is a rule whose proposed scope did not resolve against
	// the project. The rule is withheld rather than delivered unscoped, because
	// unscoped is indistinguishable from a rule that never wanted a scope.
	DiagUnresolvedScope DiagnosticKind = "unresolved-scope"
	// DiagVolume is more rules than a single file should carry.
	DiagVolume DiagnosticKind = "volume"
)

// DefaultFile is the project instruction file a store reads when a caller names
// no source, project-relative.
//
// It sits beside memory.DefaultMemoryFile rather than replacing it, because the
// two documents have different owners: that one is the agent's own learned
// project memory, this one is the project's written rules. A caller that wires
// everything gets both.
const DefaultFile = "AGENTS.md"

// Diagnostic is one finding from a compile.
//
// Diagnostics are values, not errors, because a malformed instruction file is
// not a failure: it is a file with less in it than its author intended, and the
// useful behaviour is to deliver what compiled and say what did not.
type Diagnostic struct {
	Kind DiagnosticKind
	// Source is the project-relative path the finding is in.
	Source string
	// Line and EndLine are 1-based and inclusive, and both are populated
	// whenever a finding has an extent - an unclosed fence reports where it
	// opened and where the file ended, because either number alone leaves the
	// reader guessing which half of the document is missing.
	Line    int
	EndLine int
	Message string
}

// String renders a diagnostic as one line.
func (d Diagnostic) String() string {
	if d.EndLine > d.Line {
		return fmt.Sprintf("%s:%d-%d %s: %s", d.Source, d.Line, d.EndLine, d.Kind, d.Message)
	}
	return fmt.Sprintf("%s:%d %s: %s", d.Source, d.Line, d.Kind, d.Message)
}

// Diagnostics is the ordered set of findings from one compile.
type Diagnostics []Diagnostic

// Error renders every finding, one per line, so a caller can log the lot rather
// than the first one.
func (d Diagnostics) Error() string {
	if len(d) == 0 {
		return ""
	}
	lines := make([]string, 0, len(d))
	for _, item := range d {
		lines = append(lines, item.String())
	}
	return strings.Join(lines, "\n")
}

// Has reports whether a kind of finding was reported.
func (d Diagnostics) Has(kind DiagnosticKind) bool {
	for _, item := range d {
		if item.Kind == kind {
			return true
		}
	}
	return false
}
