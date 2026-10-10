package coordinator

import (
	"ceraa/harness/instruction"
	"ceraa/harness/memory"
)

// # The observer
//
// This file is the input side of harness/instruction. That package compiles
// AGENTS.md into individually addressable rules and has a finished selection
// engine, and none of it could ever fire, because nothing constructed a
// Trigger. Select was a lookup with nothing to look up on. This is the code
// that takes a tool call and works out which rules bear on it.
//
// # The ceiling, written down so the next reader stops looking for the seam
//
// A note delivered from here is predictive for the NEXT operation and can never
// be predictive for the one being observed. That is not a shortfall in this
// code and no work on this file would remove it. It is already true of the
// runner before this code is reached:
//
//   - the request that produced these tool calls has already gone out
//     (runner.go:709 builds it, :736 streams it), so the model has already
//     decided what to call;
//   - that decision is already persisted (runner.go:924 builds the assistant
//     message, :928 appends it), so it cannot be retracted;
//   - results are appended afterwards (runner.go:996, :1009, :1022), which is
//     where a note would be written into a tool result.
//
// A rule attached to step N's tool result is therefore read by the model at
// step N+1 at the earliest. Within a single tool-call batch the ceiling is
// tighter: every call in a batch came from one model decision, so a note
// delivered on any one of them informs none of the others. An observer placed
// between the request and the decision does not exist in this architecture.
//
// That is a statement about the value on offer, not an argument for not doing
// this. A rule about the directory a run is working in is worth reading on the
// next call into it, and the whole point of moving instructions out of the
// permanent prompt is that they arrive while the work that prompted them is
// still on screen. The claim to avoid is the other one - a note that claims to
// have steered the call it is attached to.
//
// # Why the paths are derived once, here
//
// The targets come from targetsForSpec, which is the same code the memory
// sidekick runs. pathsFromCommand underneath it is a careful walk: the program
// token of each segment skipped, flags, globs and `://` dropped, workdir
// containment checked, capped at maxCommandTargets, sorted for determinism.
// Every one of those filters is a decision about what a rule is allowed to be
// about, and each was argued for in the comment above it. A second copy would
// agree until one of them changed, and then an instruction would fire on a path
// the sidekick had already decided was not a target - two subsystems making
// contradictory claims about the same operation.

// InstructionTrigger builds the observation an instruction selection runs on.
//
// Everything here is passed in rather than looked up: the tool name, whether
// the operation failed, and the session workdir are all things the runner
// already holds when it has an operation in hand, and re-deriving any of them
// would be a second answer to a question already answered.
//
// workDir is the session workdir and not the process directory, for the reason
// the tool registry resolves relative paths the same way: a relative path from
// the model means where the commands run, and resolving it anywhere else makes
// every scoped rule match nothing.
func InstructionTrigger(specType string, args map[string]any, toolName string, workDir string, failed bool) instruction.Trigger {
	return instruction.Trigger{
		Tool:    toolName,
		Targets: targetsForSpec(specType, workDir, args),
		Failed:  failed,
	}
}

// instructionMatcher adapts memory's scope matcher onto the instruction Matcher.
//
// A delegation with nothing added, on purpose. memory.Brief already answers
// "does this Applies cover this target" - whole-path globs, base-name globs,
// directory prefixes, a relative pattern against an absolute path - and
// harness/instruction/select.go names this exact function as the dependency it
// is waiting for. Re-deriving it here would produce the second implementation
// that declaration exists to prevent, and the failure it invites is specific: a
// rule fires on one call and not the next, because the two subsystems disagree
// about what a scope covers, and nothing logs that they ever did.
//
// The withheld case is untouched. A nil Matcher still withholds every scoped
// rule and still delivers the unscoped ones - that is select.go's rule and this
// adapter does not override it. Reaching for a matcher here only means a
// matcher was genuinely wanted and this is the one that exists.
func instructionMatcher(applies, target string) bool {
	return memory.MatchesScope(applies, target)
}
