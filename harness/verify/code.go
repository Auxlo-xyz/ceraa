package verify

import (
	"fmt"
	"strings"
)

// CodeRef names one endpoint of a code finding: a symbol at a 1-based line in
// a file. It is a claim, not a fact: the re-derivation below checks it against
// a fresh read rather than trusting the transcript it came from.
type CodeRef struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Symbol string `json:"symbol"`
}

// CodeFinding is a source-to-sink claim about code: untrusted data named by
// Source reaches the operation named by Sink. Kind carries the alleged class
// (taint, command execution, and the rest) into reasons only; it switches no
// behaviour, because a taxonomy of bug classes is not a validation.
type CodeFinding struct {
	Kind   string  `json:"kind"`
	Source CodeRef `json:"source"`
	Sink   CodeRef `json:"sink"`
}

// ReproduceCode re-derives a code finding from fresh reads and maps the result
// onto a Decision. The reads arrive as a function so this stays pure like
// Decide: no I/O, no clock, every input path returns. The caller does the
// reading; this decides what the reading is worth.
//
// The read function maps a path to its lines, 1-based (lines[0] is line 1). A
// read error is reported per path.
//
// The naive check this guards against is string matching: both names occur
// somewhere, so the finding is declared confirmed. The controls that stop that:
//
//   - Anchoring: each endpoint must resolve to its symbol on its line in a
//     FRESH read. A symbol that only exists in the transcript is a hallucinated
//     or stale endpoint, and the finding is refuted, not confirmed.
//   - Distinctness: source and sink must name different occurrences. A claim
//     whose ends are the same occurrence is degenerate.
//   - Link: the sink line must carry the source symbol. Direct flow is
//     checkable statically; indirect flow through intermediaries is not, and it
//     is reported as needing a runtime proof rather than confirmed or denied.
//
// Like Decide, this fails closed: anything it cannot establish is Unproven or
// Inconclusive, never a quiet pass.
func ReproduceCode(f CodeFinding, read func(path string) ([]string, error)) Decision {
	if isEmptyRef(f.Source) && isEmptyRef(f.Sink) {
		return Decision{Unproven, "no source or sink ref to check — nothing to verify"}
	}
	sourceLines, err := read(f.Source.Path)
	if err != nil {
		return Decision{Inconclusive, fmt.Sprintf("could not read source %s: %s", f.Source.Path, shortErr(err))}
	}
	sinkLines, err := read(f.Sink.Path)
	if err != nil {
		return Decision{Inconclusive, fmt.Sprintf("could not read sink %s: %s", f.Sink.Path, shortErr(err))}
	}
	if sameOccurrence(f.Source, f.Sink) {
		return Decision{Refuted, fmt.Sprintf("source and sink name the same occurrence (%s:%d %q) — the claim is degenerate",
			f.Source.Path, f.Source.Line, f.Source.Symbol)}
	}
	if reason := checkEndpoint("source", f.Source, sourceLines); reason != "" {
		return Decision{Refuted, reason}
	}
	if reason := checkEndpoint("sink", f.Sink, sinkLines); reason != "" {
		return Decision{Refuted, reason}
	}
	sinkLine := sinkLines[f.Sink.Line-1]
	if !strings.Contains(sinkLine, f.Source.Symbol) {
		return Decision{Inconclusive, fmt.Sprintf("endpoints anchored (%s:%d and %s:%d) but no direct link observed — indirect flow needs a runtime proof, held not discarded",
			f.Source.Path, f.Source.Line, f.Sink.Path, f.Sink.Line)}
	}
	return Decision{Proven, fmt.Sprintf("source %q anchored at %s:%d and sink %q anchored at %s:%d in fresh reads; sink line carries the source symbol",
		f.Source.Symbol, f.Source.Path, f.Source.Line, f.Sink.Symbol, f.Sink.Path, f.Sink.Line)}
}

func isEmptyRef(ref CodeRef) bool {
	return strings.TrimSpace(ref.Path) == "" && ref.Line <= 0 && strings.TrimSpace(ref.Symbol) == ""
}

func sameOccurrence(a, b CodeRef) bool {
	return a.Path == b.Path && a.Line == b.Line && a.Symbol != "" && a.Symbol == b.Symbol
}

// checkEndpoint verifies one endpoint against its fresh lines. Empty means
// anchored; anything else is the refutation reason.
func checkEndpoint(role string, ref CodeRef, lines []string) string {
	if ref.Line < 1 || ref.Line > len(lines) {
		return fmt.Sprintf("%s line %d out of range; %s has %d lines on fresh read",
			role, ref.Line, ref.Path, len(lines))
	}
	if !strings.Contains(lines[ref.Line-1], ref.Symbol) {
		return fmt.Sprintf("%s symbol %q absent at %s:%d on fresh read — the endpoint moved or never existed",
			role, ref.Symbol, ref.Path, ref.Line)
	}
	return ""
}

func shortErr(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 120 {
		return message[:120] + "..."
	}
	return message
}
