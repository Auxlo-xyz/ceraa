package verify

import (
	"errors"
	"testing"
)

func codeRead(files map[string][]string, failures map[string]error) func(string) ([]string, error) {
	return func(path string) ([]string, error) {
		if err, ok := failures[path]; ok {
			return nil, err
		}
		lines, ok := files[path]
		if !ok {
			return nil, errors.New("no such file: " + path)
		}
		return lines, nil
	}
}

func codeFinding() CodeFinding {
	return CodeFinding{
		Kind:   "taint",
		Source: CodeRef{Path: "input.go", Line: 3, Symbol: "userInput"},
		Sink:   CodeRef{Path: "exec.go", Line: 7, Symbol: "exec"},
	}
}

func TestReproduceCodeProvesAnchoredLinkedEndpoints(t *testing.T) {
	read := codeRead(map[string][]string{
		"input.go": {"package main", "", "userInput := r.URL.Query().Get(\"q\")"},
		"exec.go":  {"package main", "", "", "", "", "", "out := exec(userInput)"},
	}, nil)
	decision := ReproduceCode(codeFinding(), read)
	if decision.Verdict != Proven {
		t.Errorf("verdict = %s (%s), want PROVEN", decision.Verdict, decision.Reason)
	}
	if decision.Reason == "" {
		t.Error("a PROVEN without a reason teaches nobody anything")
	}
}

func TestReproduceCodeRefutesAMovedSource(t *testing.T) {
	read := codeRead(map[string][]string{
		"input.go": {"package main", "", "other := 1"},
		"exec.go":  {"package main", "", "", "", "", "", "out := exec(userInput)"},
	}, nil)
	decision := ReproduceCode(codeFinding(), read)
	if decision.Verdict != Refuted {
		t.Errorf("verdict = %s (%s), want REFUTED", decision.Verdict, decision.Reason)
	}
}

func TestReproduceCodeRefutesAShiftedLine(t *testing.T) {
	read := codeRead(map[string][]string{
		"input.go": {"package main", "", "userInput := r.URL.Query().Get(\"q\")"},
		"exec.go":  {"package main", "out := exec(userInput)"},
	}, nil)
	decision := ReproduceCode(codeFinding(), read)
	if decision.Verdict != Refuted {
		t.Errorf("verdict = %s (%s), want REFUTED for the stale line", decision.Verdict, decision.Reason)
	}
}

func TestReproduceCodeRefutesADegenerateClaim(t *testing.T) {
	finding := codeFinding()
	finding.Sink = finding.Source
	read := codeRead(map[string][]string{
		"input.go": {"package main", "", "userInput := 1"},
		"exec.go":  {},
	}, nil)
	decision := ReproduceCode(finding, read)
	if decision.Verdict != Refuted {
		t.Errorf("verdict = %s (%s), want REFUTED for identical endpoints", decision.Verdict, decision.Reason)
	}
}

func TestReproduceCodeHoldsUnlinkedEndpoints(t *testing.T) {
	read := codeRead(map[string][]string{
		"input.go": {"package main", "", "userInput := r.URL.Query().Get(\"q\")"},
		"exec.go":  {"package main", "", "", "", "", "", "out := exec(sanitized)"},
	}, nil)
	decision := ReproduceCode(codeFinding(), read)
	if decision.Verdict != Inconclusive {
		t.Errorf("verdict = %s (%s), want INCONCLUSIVE: held, not discarded and not reported", decision.Verdict, decision.Reason)
	}
}

func TestReproduceCodeIsUnprovenWithNoRefs(t *testing.T) {
	decision := ReproduceCode(CodeFinding{}, codeRead(nil, nil))
	if decision.Verdict != Unproven {
		t.Errorf("verdict = %s (%s), want UNPROVEN", decision.Verdict, decision.Reason)
	}
}

func TestReproduceCodeIsInconclusiveWhenUnreadable(t *testing.T) {
	read := codeRead(map[string][]string{
		"input.go": {"package main", "", "userInput := 1"},
	}, map[string]error{"exec.go": errors.New("permission denied")})
	decision := ReproduceCode(codeFinding(), read)
	if decision.Verdict != Inconclusive {
		t.Errorf("verdict = %s (%s), want INCONCLUSIVE: the check could not run", decision.Verdict, decision.Reason)
	}
}

func TestOnlyProvenIsReportable(t *testing.T) {
	for verdict, want := range map[Verdict]bool{Proven: true, Refuted: false, Unproven: false, Inconclusive: false} {
		if got := verdict.Reportable(); got != want {
			t.Errorf("%s reportable = %v, want %v", verdict, got, want)
		}
	}
}
