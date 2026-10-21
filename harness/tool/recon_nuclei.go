package tool

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/nuclei"
	"ceraa/harness/operation"
)

// The bounds the tool clamps the model's ask to, before it becomes a run.
const (
	defaultNucleiRate        = 10
	hardNucleiRate           = 100
	defaultNucleiConcurrency = 4
	hardNucleiConcurrency    = 32
	defaultNucleiTimeout     = 15
	hardNucleiTimeout        = 120
	defaultNucleiMaxRequests = 200
	hardNucleiMaxRequests    = 5000
	defaultNucleiMaxFindings = 50
	hardNucleiMaxFindings    = 500
	// maxNucleiLines bounds the finding lines the tool prints. A run with
	// five hundred findings is a run whose findings nobody reads.
	maxNucleiLines = 50
	// maxNucleiEvidence bounds the excerpt printed beside a finding.
	maxNucleiEvidence = 100
)

// NucleiTool is the model-facing name of the Nuclei template runner. It only
// shapes arguments: the templates, the gate, the budget, the receipts and
// the run itself live in harness/nuclei.
type NucleiTool struct{}

func (n *NucleiTool) Name() string { return "nuclei" }

func (n *NucleiTool) Description() string {
	return "Run Nuclei-format HTTP templates against a target and report what matched. The templates are Nuclei YAML, so the upstream corpus and a directory of your own both work, and the HTTP template types are the ones that are supported. DSL matchers and the non-HTTP template types (network, dns, headless, ssl, whois, javascript, code) are skipped, and the summary says which templates were skipped and why. Every request is scope gated, charged to the engagement budget and written a receipt, so a finding cites the request that produced it. Returns one bounded line per finding with the template, the matcher that fired, the status and the receipt id, then a summary of the templates loaded, the templates skipped, the requests fired and the findings."
}

func (n *NucleiTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target":       map[string]any{"type": "string", "description": "The base url to scan, or a file of targets with one url per line"},
			"templates":    map[string]any{"type": "string", "description": "A directory or file of Nuclei templates. Empty uses the default search plus the embedded set"},
			"severity":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Keep only these severities: info, low, medium, high, critical"},
			"tags":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Keep only the templates carrying one of these tags"},
			"template_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Keep only these template ids"},
			"rate":         map[string]any{"type": "integer", "minimum": 1, "maximum": hardNucleiRate, "description": fmt.Sprintf("Requests per second, default %d", defaultNucleiRate)},
			"concurrency":  map[string]any{"type": "integer", "minimum": 1, "maximum": hardNucleiConcurrency, "description": fmt.Sprintf("Parallel requests, default %d", defaultNucleiConcurrency)},
			"timeout":      map[string]any{"type": "integer", "minimum": 1, "maximum": hardNucleiTimeout, "description": fmt.Sprintf("Per-request timeout in seconds, default %d", defaultNucleiTimeout)},
			"max_requests": map[string]any{"type": "integer", "minimum": 1, "maximum": hardNucleiMaxRequests, "description": fmt.Sprintf("Hard ceiling on requests fired, default %d", defaultNucleiMaxRequests)},
			"max_findings": map[string]any{"type": "integer", "minimum": 1, "maximum": hardNucleiMaxFindings, "description": fmt.Sprintf("Hard ceiling on findings, default %d", defaultNucleiMaxFindings)},
		},
		"required": []string{"target"},
	}
}

// Translate validates the shape and re-emits the canonical argument map.
func (n *NucleiTool) Translate(args map[string]any) (operation.Spec, error) {
	target := strings.TrimSpace(nucleiStringArg(args["target"]))
	if target == "" {
		return operation.Spec{}, fmt.Errorf("target is required")
	}
	out := map[string]any{"target": target}
	if templates := strings.TrimSpace(nucleiStringArg(args["templates"])); templates != "" {
		out["templates"] = templates
	}
	if severity := nucleiStringList(args["severity"]); len(severity) > 0 {
		out["severity"] = severity
	}
	if tags := nucleiStringList(args["tags"]); len(tags) > 0 {
		out["tags"] = tags
	}
	if ids := nucleiStringList(args["template_ids"]); len(ids) > 0 {
		out["template_ids"] = ids
	}
	out["rate"] = nucleiIntArg(args["rate"], defaultNucleiRate, hardNucleiRate)
	out["concurrency"] = nucleiIntArg(args["concurrency"], defaultNucleiConcurrency, hardNucleiConcurrency)
	out["timeout"] = nucleiIntArg(args["timeout"], defaultNucleiTimeout, hardNucleiTimeout)
	out["max_requests"] = nucleiIntArg(args["max_requests"], defaultNucleiMaxRequests, hardNucleiMaxRequests)
	out["max_findings"] = nucleiIntArg(args["max_findings"], defaultNucleiMaxFindings, hardNucleiMaxFindings)
	return operation.Spec{Type: "nuclei", Args: out}, nil
}

func (n *NucleiTool) TranslateResult(op operation.Operation) string { return op.Output }

func (n *NucleiTool) Run(spec operation.Spec) (string, error) {
	return n.RunContext(context.Background(), spec)
}

func (n *NucleiTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	target := strings.TrimSpace(nucleiStringArg(spec.Args["target"]))
	if target == "" {
		return "", fmt.Errorf("target is required")
	}
	// A relative path from the model means "where my commands run", which
	// is the session workdir and not the process directory. The resolved
	// path goes in the error so a retry can correct itself.
	base, _ := spec.Args["workdir"].(string)
	if !isURL(target) {
		resolved, err := ResolvePathFrom(base, target)
		if err != nil {
			return "", fmt.Errorf("target %s: %w", target, err)
		}
		target = resolved
	}
	templatesPath := strings.TrimSpace(nucleiStringArg(spec.Args["templates"]))
	if templatesPath != "" {
		resolved, err := ResolvePathFrom(base, templatesPath)
		if err != nil {
			return "", fmt.Errorf("templates %s: %w", templatesPath, err)
		}
		templatesPath = resolved
	}

	templates, loadErrors := loadNucleiTemplates(templatesPath)
	if len(templates) == 0 {
		return "", fmt.Errorf("no nuclei templates could be loaded: %s", nucleiLoadFailure(loadErrors))
	}

	opts := nuclei.Options{
		Target:      target,
		Templates:   templates,
		Severities:  nucleiStringList(spec.Args["severity"]),
		Tags:        nucleiStringList(spec.Args["tags"]),
		TemplateIDs: nucleiStringList(spec.Args["template_ids"]),
		RateLimit:   nucleiIntArg(spec.Args["rate"], defaultNucleiRate, hardNucleiRate),
		Concurrency: nucleiIntArg(spec.Args["concurrency"], defaultNucleiConcurrency, hardNucleiConcurrency),
		Timeout:     nucleiIntArg(spec.Args["timeout"], defaultNucleiTimeout, hardNucleiTimeout),
		MaxRequests: nucleiIntArg(spec.Args["max_requests"], defaultNucleiMaxRequests, hardNucleiMaxRequests),
		MaxFindings: nucleiIntArg(spec.Args["max_findings"], defaultNucleiMaxFindings, hardNucleiMaxFindings),
	}
	report, err := nuclei.RunDetailed(ctx, opts)
	if err != nil {
		return "", err
	}
	return formatNucleiReport(report, loadErrors), nil
}

// loadNucleiTemplates reads the templates the caller asked for, or the
// default set.
func loadNucleiTemplates(path string) ([]nuclei.Template, []error) {
	if strings.TrimSpace(path) == "" {
		return nuclei.LoadDefault()
	}
	return nuclei.Load(path)
}

// isURL reports whether a target is a url rather than a path to a list.
func isURL(target string) bool {
	lower := strings.ToLower(target)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// nucleiLoadFailure names why the templates could not be loaded.
func nucleiLoadFailure(errs []error) string {
	if len(errs) == 0 {
		return "the template set is empty"
	}
	return errs[0].Error()
}

// formatNucleiReport renders the run: one bounded line per finding, then the
// summary of what was loaded, what was skipped and why, what was fired and
// what was found.
func formatNucleiReport(report nuclei.Report, loadErrors []error) string {
	var b strings.Builder
	printed := 0
	for _, finding := range report.Findings {
		if printed >= maxNucleiLines {
			fmt.Fprintf(&b, "omitting %d further findings: %d line cap reached\n",
				len(report.Findings)-printed, maxNucleiLines)
			break
		}
		printed++
		fmt.Fprintf(&b, "%s %s %s - %s status %d %s",
			nucleiSeverity(finding.Severity),
			finding.TemplateID,
			nucleiClip(finding.Name, 60),
			nucleiClip(finding.MatchedAt, 160),
			finding.Status,
			nucleiClip(finding.Matcher, 120),
		)
		if evidence := nucleiClip(finding.Evidence, maxNucleiEvidence); evidence != "" {
			fmt.Fprintf(&b, " | %s", evidence)
		}
		if len(finding.EmptyVars) > 0 {
			fmt.Fprintf(&b, " | empty vars: %s", nucleiClip(strings.Join(finding.EmptyVars, ", "), 120))
		}
		if finding.CatchAll != "" {
			fmt.Fprintf(&b, " | catch-all: %s", finding.CatchAll)
		}
		if finding.ReceiptID != "" {
			fmt.Fprintf(&b, " receipt %s", finding.ReceiptID)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "nuclei: %d templates loaded, %d skipped, %d requests fired, %d findings\n",
		report.Templates, report.Skipped, report.Requests, len(report.Findings))
	for _, reason := range report.SkipReasons {
		fmt.Fprintf(&b, "skipped: %s\n", reason)
	}
	for _, message := range report.Errors {
		fmt.Fprintf(&b, "error: %s\n", message)
	}
	for _, err := range loadErrors {
		fmt.Fprintf(&b, "load error: %s\n", err)
	}
	if report.Refused > 0 {
		fmt.Fprintf(&b, "refused by scope: %d\n", report.Refused)
	}
	if report.Stopped != "" {
		fmt.Fprintf(&b, "stopped: %s\n", report.Stopped)
	}
	return strings.TrimRight(b.String(), "\n")
}

// nucleiSeverity renders the severity of a finding, and says "unknown"
// rather than printing an empty column.
func nucleiSeverity(severity string) string {
	trimmed := strings.ToLower(strings.TrimSpace(severity))
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}

// nucleiClip bounds a value at a character boundary.
func nucleiClip(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "..."
}

// nucleiStringArg reads a string argument without the zero-value trap.
func nucleiStringArg(v any) string {
	s, _ := v.(string)
	return s
}

// nucleiIntArg reads an integer argument, falling back when it is absent or
// unusable and clamping it to the hard bound.
func nucleiIntArg(v any, fallback, hard int) int {
	value := fallback
	switch typed := v.(type) {
	case int:
		if typed > 0 {
			value = typed
		}
	case float64:
		if typed > 0 && typed == float64(int(typed)) {
			value = int(typed)
		}
	}
	if value > hard {
		value = hard
	}
	return value
}

// nucleiStringList reads a list of strings in both the native and the
// JSON-decoded shapes.
func nucleiStringList(v any) []string {
	switch typed := v.(type) {
	case nil:
		return nil
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			if s, ok := entry.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	default:
		return nil
	}
}
