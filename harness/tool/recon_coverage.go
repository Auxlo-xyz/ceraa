package tool

// recon_coverage.go is the memory of a sweep.
//
// A model that cannot see what it already tried will ask the same question
// twice, and a long engagement is mostly questions. The ledger records the
// attempts so the next question can be the one that was not asked, and the
// gaps are the checklist it has not covered yet.

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/coverage"
	"ceraa/harness/operation"
	"ceraa/internal/paths"
)

// CoverageTool records and reads the coverage ledger.
type CoverageTool struct{}

func (c *CoverageTool) Name() string { return "coverage" }

func (c *CoverageTool) Description() string {
	return "Record what a sweep has tried, and read it back. mode gaps names the techniques from the review catalogue that were never tried against a target."
}

func (c *CoverageTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode":      map[string]any{"type": "string", "enum": []string{"record", "list", "gaps", "catalogue"}, "description": "record, list, gaps or catalogue"},
			"technique": map[string]any{"type": "string", "description": "What was tried, as cors or idor"},
			"target":    map[string]any{"type": "string", "description": "What it was tried against"},
			"detail":    map[string]any{"type": "string", "description": "How it was tried"},
			"outcome":   map[string]any{"type": "string", "enum": []string{"clean", "finding", "blocked", "skipped"}, "description": "clean, finding, blocked or skipped"},
			"note":      map[string]any{"type": "string", "description": "A note worth remembering"},
		},
		"required": []string{},
	}
}

// Translate validates the shape and re-emits the canonical argument map.
func (c *CoverageTool) Translate(args map[string]any) (operation.Spec, error) {
	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = "record"
	}
	switch mode {
	case "record", "list", "gaps", "catalogue":
	default:
		return operation.Spec{}, fmt.Errorf("mode %q is not one of record, list, gaps, catalogue", mode)
	}
	out := map[string]any{"mode": mode}
	for _, key := range []string{"technique", "target", "detail", "outcome", "note"} {
		if value, ok := args[key].(string); ok && value != "" {
			out[key] = value
		}
	}
	if mode == "record" {
		if _, ok := out["technique"]; !ok {
			return operation.Spec{}, fmt.Errorf("mode record needs a technique")
		}
		if _, ok := out["target"]; !ok {
			return operation.Spec{}, fmt.Errorf("mode record needs a target")
		}
	}
	return operation.Spec{Type: "coverage", Args: out}, nil
}

func (c *CoverageTool) TranslateResult(op operation.Operation) string { return op.Output }

func (c *CoverageTool) Run(spec operation.Spec) (string, error) {
	return c.RunContext(context.Background(), spec)
}

func (c *CoverageTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	store, err := coverage.Open(coverageDir())
	if err != nil {
		return "", err
	}
	mode, _ := spec.Args["mode"].(string)
	switch mode {
	case "catalogue":
		return "catalogue: " + strings.Join(coverage.Catalogue(), ", "), nil
	case "list":
		technique, _ := spec.Args["technique"].(string)
		target, _ := spec.Args["target"].(string)
		entries, err := store.List(technique, target)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "coverage: %d entries\n", len(entries))
		for _, entry := range entries {
			fmt.Fprintf(&b, "%s %s %s %s", entry.Technique, entry.Outcome, entry.Target, entry.At.Format("2006-01-02T15:04:05Z"))
			if entry.Detail != "" {
				fmt.Fprintf(&b, " - %s", entry.Detail)
			}
			if entry.Note != "" {
				fmt.Fprintf(&b, " | %s", entry.Note)
			}
			b.WriteString("\n")
		}
		return strings.TrimRight(b.String(), "\n"), nil
	case "gaps":
		target, _ := spec.Args["target"].(string)
		gaps, err := store.Gaps(target, nil)
		if err != nil {
			return "", err
		}
		if len(gaps) == 0 {
			return "coverage: every technique in the catalogue was tried against " + target, nil
		}
		return fmt.Sprintf("coverage: %d techniques not tried against %s: %s", len(gaps), target, strings.Join(gaps, ", ")), nil
	default:
		entry := coverage.Entry{
			Technique: coverageStringArg(spec.Args["technique"]),
			Target:    coverageStringArg(spec.Args["target"]),
			Detail:    coverageStringArg(spec.Args["detail"]),
			Outcome:   coverage.Outcome(coverageStringArg(spec.Args["outcome"])),
			Note:      coverageStringArg(spec.Args["note"]),
		}
		saved, err := store.Record(entry)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("recorded %s %s against %s", saved.Technique, saved.Outcome, saved.Target), nil
	}
}

// coverageDir is where the ledger lives.
func coverageDir() string {
	return paths.DataDir() + "/coverage"
}

// stringArg reads a string argument without the zero-value trap.
func coverageStringArg(value any) string {
	text, _ := value.(string)
	return text
}
