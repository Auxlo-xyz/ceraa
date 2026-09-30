package coordinator

import (
	"strings"
	"sync"
)

// Per-tool error accounting.
//
// The point is to separate the errors a tool is expected to produce from the ones
// that mean the harness is broken, because they need opposite responses. A model
// asking for a file that does not exist is the tool working. A tool that cannot
// encode its own arguments, or a registry entry with no executor, is a defect and
// will not fix itself.
//
// A real session had 3 of 17 bash calls fail with the same "command not found",
// for a binary that is not installed on the host. Nothing counted it: the calls
// were indistinguishable from successes in any log, and the repetition is exactly
// the kind of thing a rate would have surfaced immediately.
type ToolErrorClass string

const (
	// ErrExpected covers the ordinary ways a well-formed call fails: the target
	// is absent, the arguments are wrong, the thing timed out, the user stopped
	// it. The tool did its job.
	ErrExpected ToolErrorClass = "expected"
	// ErrUnknown is everything else, and is treated as a harness bug until shown
	// otherwise. An unclassified error is deliberately the loud bucket, so a new
	// failure mode surfaces as "unknown" rather than joining the expected pile
	// and disappearing into the rate.
	ErrUnknown ToolErrorClass = "unknown"
	// ErrCanceled is separate because it is neither a tool problem nor a bug: the
	// run stopped, usually because the user intervened.
	ErrCanceled ToolErrorClass = "canceled"
)

// ToolErrorStats is the process-wide tally. A run is not the unit of interest for
// this, the same reasoning as the trim stats: a rare failure is rare because
// nothing triggers it, not because the runs that would trigger it are few.
type ToolErrorStats struct {
	// Calls and Failures are per tool name.
	Calls    map[string]int64
	Failures map[string]int64
	// ByClass counts every failure by class, across all tools.
	ByClass map[ToolErrorClass]int64
	// Samples keeps a few real messages per class, because a rate says how often
	// and not what it looks like, and "expected" covering both a missing file and
	// a provider outage is the distinction that matters.
	Samples map[ToolErrorClass][]string
}

const maxErrorSamples = 5

var (
	toolErrorMu    sync.Mutex
	toolErrorStats = ToolErrorStats{
		Calls:    map[string]int64{},
		Failures: map[string]int64{},
		ByClass:  map[ToolErrorClass]int64{},
		Samples:  map[ToolErrorClass][]string{},
	}
)

// recordToolCall counts an invocation, successful or not.
func recordToolCall(name string) {
	toolErrorMu.Lock()
	defer toolErrorMu.Unlock()
	toolErrorStats.Calls[name]++
}

// recordToolError counts a failure and files it under a class.
func recordToolError(name, message string) {
	class := classifyToolError(message)
	toolErrorMu.Lock()
	defer toolErrorMu.Unlock()
	toolErrorStats.Failures[name]++
	toolErrorStats.ByClass[class]++
	if len(toolErrorStats.Samples[class]) < maxErrorSamples {
		toolErrorStats.Samples[class] = append(toolErrorStats.Samples[class], name+": "+truncate(message, 160))
	}
}

// classifyToolError sorts a failure into the bucket that decides what to do about
// it.
//
// Ordered from most specific to least, and the first match wins, so the specific
// forms a model produces are named before the general ones. Anything unrecognised
// is unknown on purpose: a new failure mode showing up as "expected" is how a
// real bug gets filed as noise.
func classifyToolError(message string) ToolErrorClass {
	m := strings.ToLower(strings.TrimSpace(message))
	switch {
	case m == "":
		return ErrUnknown
	case m == "canceled" || strings.Contains(m, "context canceled"):
		return ErrCanceled

	// A binary that is not installed. Worth its own case because it is the one
	// that repeats: a model prefers the tool it knows, and the failure is a
	// hint about the prompt rather than about the shell.
	case strings.Contains(m, "command not found"),
		strings.Contains(m, "executable file not found"),
		strings.Contains(m, "no such file or directory"):
		return ErrExpected

	// A command that ran and reported a non-zero status. This is the single most
	// common shell outcome and it is not a defect: grep with no match, a failing
	// test, a diff that differs. Left unclassified it fills the unknown bucket
	// with the noise the bucket exists to exclude, and the rate that should be zero
	// stops being a signal. What the command printed is in the output, not here.
	case strings.Contains(m, "exit status"),
		strings.Contains(m, "exit code"):
		return ErrExpected

	// The ordinary shape of a call that could not be made.
	case strings.Contains(m, "no such file"),
		strings.Contains(m, "not found"),
		strings.Contains(m, "does not exist"),
		strings.Contains(m, "is required"),
		strings.Contains(m, "is invalid"),
		strings.Contains(m, "must be"),
		// Argument validation in the tools' own words: "edit 0 has no old_text",
		// "path has no value". A missing argument is the caller's mistake and the
		// tool refusing it correctly.
		strings.Contains(m, "has no "),
		strings.Contains(m, "unknown tool"),
		strings.Contains(m, "permission denied"),
		strings.Contains(m, "timed out"),
		strings.Contains(m, "timeout"),
		strings.Contains(m, "deadline exceeded"):
		return ErrExpected

	// Transport and provider trouble: expected in the sense that a retry is the
	// right response, and worth seeing because a run of them is a cost problem.
	case strings.Contains(m, "connection reset"),
		strings.Contains(m, "eof"),
		strings.Contains(m, "429"),
		strings.Contains(m, "rate limit"),
		strings.Contains(m, "500"),
		strings.Contains(m, "502"),
		strings.Contains(m, "503"),
		strings.Contains(m, "overloaded"),
		strings.Contains(m, "bad gateway"):
		return ErrExpected

	default:
		return ErrUnknown
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ToolErrors returns a copy of the tally, for reporting.
func ToolErrors() ToolErrorStats {
	toolErrorMu.Lock()
	defer toolErrorMu.Unlock()
	out := ToolErrorStats{
		Calls:    make(map[string]int64, len(toolErrorStats.Calls)),
		Failures: make(map[string]int64, len(toolErrorStats.Failures)),
		ByClass:  make(map[ToolErrorClass]int64, len(toolErrorStats.ByClass)),
		Samples:  make(map[ToolErrorClass][]string, len(toolErrorStats.Samples)),
	}
	for k, v := range toolErrorStats.Calls {
		out.Calls[k] = v
	}
	for k, v := range toolErrorStats.Failures {
		out.Failures[k] = v
	}
	for k, v := range toolErrorStats.ByClass {
		out.ByClass[k] = v
	}
	for k, v := range toolErrorStats.Samples {
		out.Samples[k] = append([]string(nil), v...)
	}
	return out
}

// ErrorRate is the share of a tool's calls that failed. Reported per tool and
// overall, because a low rate on a tool nobody calls is not a good tool.
func (s ToolErrorStats) ErrorRate(name string) float64 {
	calls := s.Calls[name]
	if calls == 0 {
		return 0
	}
	return float64(s.Failures[name]) / float64(calls)
}

// UnknownRate is the share of all failures that landed in the bucket that means a
// harness bug. This is the number to watch: it should be zero, and every
// non-zero reading is a defect with a sample attached.
func (s ToolErrorStats) UnknownRate() float64 {
	total := int64(0)
	for _, v := range s.ByClass {
		total += v
	}
	if total == 0 {
		return 0
	}
	return float64(s.ByClass[ErrUnknown]) / float64(total)
}
