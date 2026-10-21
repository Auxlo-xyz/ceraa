package tool

import (
	"strings"
	"testing"

	"ceraa/harness/nuclei"
	"ceraa/harness/operation"
)

// TestNucleiTranslateRequiresTheTarget: without it there is no operation,
// and the error says so rather than failing later at the run.
func TestNucleiTranslateRequiresTheTarget(t *testing.T) {
	tl := &NucleiTool{}
	for _, args := range []map[string]any{
		{},
		{"target": ""},
		{"target": "   "},
		{"target": 42},
	} {
		if spec, err := tl.Translate(args); err == nil {
			t.Errorf("Translate(%v) accepted, spec = %v", args, spec)
		}
	}
}

// TestNucleiTranslateBuildsTheSpec: the operation type is the registered
// tool name, because the runner resolves the executor by it.
func TestNucleiTranslateBuildsTheSpec(t *testing.T) {
	tl := &NucleiTool{}
	spec, err := tl.Translate(map[string]any{
		"target":       "https://example.com",
		"templates":    "/tmp/templates",
		"severity":     []any{"high", "critical"},
		"tags":         []any{"exposure"},
		"template_ids": []any{"exposed-env-file"},
		"rate":         float64(5),
		"concurrency":  float64(8),
		"timeout":      float64(20),
		"max_requests": float64(50),
		"max_findings": float64(10),
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if spec.Type != "nuclei" {
		t.Errorf("spec.Type = %q, want nuclei", spec.Type)
	}
	if spec.Args["target"] != "https://example.com" {
		t.Errorf("spec.Args[target] = %v", spec.Args["target"])
	}
	if spec.Args["templates"] != "/tmp/templates" {
		t.Errorf("spec.Args[templates] = %v", spec.Args["templates"])
	}
	if rate, _ := spec.Args["rate"].(int); rate != 5 {
		t.Errorf("spec.Args[rate] = %v, want 5", spec.Args["rate"])
	}
	if requests, _ := spec.Args["max_requests"].(int); requests != 50 {
		t.Errorf("spec.Args[max_requests] = %v, want 50", spec.Args["max_requests"])
	}
	severity, _ := spec.Args["severity"].([]string)
	if len(severity) != 2 || severity[0] != "high" {
		t.Errorf("spec.Args[severity] = %v", spec.Args["severity"])
	}
}

// TestNucleiTranslateOmitsWhatWasNotAskedFor: an absent argument is not put
// in the spec.
func TestNucleiTranslateOmitsWhatWasNotAskedFor(t *testing.T) {
	tl := &NucleiTool{}
	spec, err := tl.Translate(map[string]any{"target": "https://example.com"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	for _, name := range []string{"templates", "severity", "tags", "template_ids"} {
		if _, ok := spec.Args[name]; ok {
			t.Errorf("an absent %q was put in the spec: %v", name, spec.Args)
		}
	}
}

// TestNucleiName pins the registry name, which is also the operation type.
func TestNucleiName(t *testing.T) {
	tl := &NucleiTool{}
	if tl.Name() != "nuclei" {
		t.Errorf("Name = %q, want nuclei", tl.Name())
	}
	spec, err := tl.Translate(map[string]any{"target": "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Type != tl.Name() {
		t.Errorf("spec.Type = %q and Name = %q; the runner resolves the executor by the type", spec.Type, tl.Name())
	}
}

// TestNucleiDescriptionNamesTheContract: the model reads this, so it has to
// say what is supported, what is skipped and that every request is gated
// and receipted.
func TestNucleiDescriptionNamesTheContract(t *testing.T) {
	description := (&NucleiTool{}).Description()
	for _, want := range []string{"Nuclei", "HTTP template types", "DSL", "skipped", "scope gated", "receipt"} {
		if !strings.Contains(description, want) {
			t.Errorf("the description does not mention %q: %s", want, description)
		}
	}
}

// TestNucleiSchemaCoversTheArguments: a property the tool reads but the
// schema does not declare is refused by the harness before the tool runs.
func TestNucleiSchemaCoversTheArguments(t *testing.T) {
	tl := &NucleiTool{}
	schema := tl.Schema()
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range []string{
		"target", "templates", "severity", "tags", "template_ids",
		"rate", "concurrency", "timeout", "max_requests", "max_findings",
	} {
		if _, ok := properties[name]; !ok {
			t.Errorf("the schema does not declare %q", name)
		}
	}
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "target" {
		t.Errorf("required = %v, want target", required)
	}
	args := map[string]any{
		"target":       "https://example.com",
		"templates":    "/tmp/templates",
		"severity":     []any{"high"},
		"tags":         []any{"exposure"},
		"template_ids": []any{"exposed-env-file"},
		"rate":         float64(5),
		"concurrency":  float64(8),
		"timeout":      float64(20),
		"max_requests": float64(50),
		"max_findings": float64(10),
	}
	if err := ValidateArguments(schema, args); err != nil {
		t.Errorf("the harness refuses the declared arguments: %v", err)
	}
}

// TestNucleiTranslateResultPassesOutput: result translation is a pass
// through of the recorded output.
func TestNucleiTranslateResultPassesOutput(t *testing.T) {
	tl := &NucleiTool{}
	op := operation.Operation{Output: "nuclei: 1 templates loaded, 0 skipped, 1 requests fired, 0 findings"}
	if got := tl.TranslateResult(op); got != op.Output {
		t.Errorf("TranslateResult = %q, want passthrough", got)
	}
}

// TestFormatNucleiReportCarriesTheFindingsAndTheSummary: the model has to
// be able to read the reason of a finding and the state of the run.
func TestFormatNucleiReportCarriesTheFindingsAndTheSummary(t *testing.T) {
	report := nuclei.Report{
		Templates: 13,
		Skipped:   1,
		Requests:  41,
		SkipReasons: []string{
			"missing-hsts-header: the template checks the strict-transport-security header, which the harness's probe does not keep",
		},
		Errors: []string{"one: every request failed: connection refused"},
		Findings: []nuclei.Finding{{
			TemplateID: "exposed-env-file",
			Name:       "Exposed .env File",
			Severity:   "critical",
			MatchedAt:  "https://example.com/.env",
			Matcher:    `word "APP_KEY=" in body`,
			Evidence:   "APP_KEY=base64:secret",
			Request:    "GET https://example.com/.env\nCookie: REDACTED",
			Status:     200,
			ReceiptID:  "receipt-1.json",
		}},
	}
	out := formatNucleiReport(report, nil)
	for _, want := range []string{
		"critical", "exposed-env-file", "Exposed .env File", "https://example.com/.env",
		"status 200", "APP_KEY", "receipt-1.json",
		"13 templates loaded", "1 skipped", "41 requests fired", "1 findings",
		"skipped:", "error:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not carry %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Cookie: REDACTED") {
		t.Errorf("the output printed the recorded request, which is not what the line carries:\n%s", out)
	}
}

// TestFormatNucleiReportIsBounded: a body is never printed and the line
// count is capped.
func TestFormatNucleiReportIsBounded(t *testing.T) {
	report := nuclei.Report{Findings: []nuclei.Finding{}}
	for i := 0; i < maxNucleiLines+20; i++ {
		report.Findings = append(report.Findings, nuclei.Finding{
			TemplateID: "template",
			Name:       "A template",
			Severity:   "info",
			MatchedAt:  "https://example.com/",
			Matcher:    `word "a" in body`,
			Evidence:   strings.Repeat("x", 4096),
			Status:     200,
		})
	}
	out := formatNucleiReport(report, nil)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > maxNucleiLines+3 {
		t.Errorf("the output is %d lines, want it capped", len(lines))
	}
	if !strings.Contains(out, "line cap reached") {
		t.Errorf("the output does not say that findings were left out:\n%s", out)
	}
	for _, line := range lines {
		if len(line) > 800 {
			t.Errorf("a line is %d bytes, want it bounded", len(line))
		}
	}
}
