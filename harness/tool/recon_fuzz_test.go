package tool

import (
	"strings"
	"testing"

	"ceraa/harness/operation"
	"ceraa/harness/scan"
)

// TestFuzzTranslateRequiresURLAndParam: without them there is no operation,
// and the error says so rather than failing later at the run.
func TestFuzzTranslateRequiresURLAndParam(t *testing.T) {
	tl := &FuzzTool{}
	for _, args := range []map[string]any{
		{},
		{"url": "https://example.com"},
		{"param": "id"},
		{"url": "", "param": "id"},
		{"url": "https://example.com", "param": 42},
	} {
		if spec, err := tl.Translate(args); err == nil {
			t.Errorf("Translate(%v) accepted, spec = %v", args, spec)
		}
	}
}

// TestFuzzTranslateBuildsTheSpec: the operation type is the registered tool
// name, because the runner resolves the executor by it.
func TestFuzzTranslateBuildsTheSpec(t *testing.T) {
	tl := &FuzzTool{}

	spec, err := tl.Translate(map[string]any{"url": "https://example.com/s", "param": "id"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if spec.Type != "fuzz" {
		t.Errorf("spec.Type = %q, want fuzz", spec.Type)
	}
	if spec.Args["url"] != "https://example.com/s" {
		t.Errorf("spec.Args[url] = %v", spec.Args["url"])
	}
	if spec.Args["param"] != "id" {
		t.Errorf("spec.Args[param] = %v", spec.Args["param"])
	}
	if method, _ := spec.Args["method"].(string); method != "GET" {
		t.Errorf("spec.Args[method] = %v, want GET", spec.Args["method"])
	}
	if _, ok := spec.Args["location"]; ok {
		t.Errorf("an absent location was put in the spec: %v", spec.Args)
	}
}

// TestFuzzTranslateRefusesWhatItCannotDo: the verb and the location are
// checked in the tool as well as in the package.
func TestFuzzTranslateRefusesWhatItCannotDo(t *testing.T) {
	tl := &FuzzTool{}
	for _, args := range []map[string]any{
		{"url": "https://example.com", "param": "id", "method": "DELETE"},
		{"url": "https://example.com", "param": "id", "method": "PUT"},
		{"url": "https://example.com", "param": "id", "location": "header"},
	} {
		if _, err := tl.Translate(args); err == nil {
			t.Errorf("Translate(%v) accepted", args)
		}
	}
}

// TestFuzzTranslateReadsEveryArgument: the canonical argument map carries
// what the caller asked for.
func TestFuzzTranslateReadsEveryArgument(t *testing.T) {
	tl := &FuzzTool{}
	spec, err := tl.Translate(map[string]any{
		"url":          "https://example.com/s",
		"param":        "id",
		"method":       "POST",
		"location":     "body",
		"wordlist":     "values.txt",
		"marker":       "sqlstate",
		"body":         "id={PARAM}",
		"concurrency":  float64(4),
		"max_requests": float64(50),
		"timeout":      float64(5),
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if spec.Args["method"] != "POST" || spec.Args["location"] != "body" {
		t.Errorf("spec.Args = %v", spec.Args)
	}
	if spec.Args["marker"] != "sqlstate" || spec.Args["body"] != "id={PARAM}" {
		t.Errorf("spec.Args = %v", spec.Args)
	}
	if requests, _ := spec.Args["max_requests"].(int); requests != 50 {
		t.Errorf("spec.Args[max_requests] = %v", spec.Args["max_requests"])
	}
	if timeout, _ := spec.Args["timeout"].(int); timeout != 5 {
		t.Errorf("spec.Args[timeout] = %v", spec.Args["timeout"])
	}
}

// TestFuzzName pins the registry name, which is also the operation type.
func TestFuzzName(t *testing.T) {
	tl := &FuzzTool{}
	if tl.Name() != "fuzz" {
		t.Errorf("Name = %q, want fuzz", tl.Name())
	}
	spec, err := tl.Translate(map[string]any{"url": "https://example.com", "param": "id"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Type != tl.Name() {
		t.Errorf("spec.Type = %q and Name = %q; the runner resolves the executor by the type", spec.Type, tl.Name())
	}
}

// TestFuzzDescriptionNamesTheContract: the model reads this, so it has to
// say that the requests are gated and receipted, and that a delta is not a
// finding.
func TestFuzzDescriptionNamesTheContract(t *testing.T) {
	description := (&FuzzTool{}).Description()
	for _, want := range []string{"scope-gated", "receipt", "delta"} {
		if !strings.Contains(description, want) {
			t.Errorf("the description does not mention %q: %s", want, description)
		}
	}
}

// TestFuzzSchemaCoversTheArguments: a property the tool reads but the
// schema does not declare is refused by the harness before the tool runs.
func TestFuzzSchemaCoversTheArguments(t *testing.T) {
	tl := &FuzzTool{}
	schema := tl.Schema()
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range []string{
		"url", "param", "method", "location", "wordlist", "marker", "body",
		"concurrency", "max_requests", "timeout",
	} {
		if _, ok := properties[name]; !ok {
			t.Errorf("the schema does not declare %q", name)
		}
	}
	required, _ := schema["required"].([]string)
	if len(required) != 2 {
		t.Errorf("required = %v, want url and param", required)
	}
	args := map[string]any{
		"url":          "https://example.com/s",
		"param":        "id",
		"method":       "POST",
		"location":     "body",
		"wordlist":     "values.txt",
		"marker":       "sqlstate",
		"body":         "id={PARAM}",
		"concurrency":  float64(4),
		"max_requests": float64(50),
		"timeout":      float64(5),
	}
	if err := ValidateArguments(schema, args); err != nil {
		t.Errorf("the harness refuses the declared arguments: %v", err)
	}
}

// TestFuzzTranslateResultPassesOutput: result translation is a pass
// through of the recorded output.
func TestFuzzTranslateResultPassesOutput(t *testing.T) {
	tl := &FuzzTool{}
	op := operation.Operation{Output: "fuzz: GET https://example.com/s param id - 3 requests fired, 0 hits"}
	if got := tl.TranslateResult(op); got != op.Output {
		t.Errorf("TranslateResult = %q, want passthrough", got)
	}
}

// TestFormatFuzzResultSaysADeltaIsNotAFinding: the difference between a
// marker hit and a delta has to survive into the output.
func TestFormatFuzzResultSaysADeltaIsNotAFinding(t *testing.T) {
	result := scan.FuzzResult{
		Hits: []scan.FuzzHit{
			{Value: "admin", Status: 500, Size: 91, Marker: true, SHA256: "0123456789abcdef"},
			{Value: "guest", Status: 403, Size: 12, Delta: true},
		},
		Requests:       21,
		Values:         20,
		HasBaseline:    true,
		BaselineValue:  "zzq7xdeadbeef",
		BaselineStatus: 404,
		BaselineSize:   256,
	}
	out := formatFuzzResult(result, scan.FuzzOptions{URL: "https://example.com/s", Param: "id"})
	if !strings.Contains(out, "marker") || !strings.Contains(out, "delta") {
		t.Errorf("the output does not tell the two kinds apart:\n%s", out)
	}
	if !strings.Contains(out, "not a confirmed finding") {
		t.Errorf("the output does not say a delta is not a finding:\n%s", out)
	}
	if !strings.Contains(out, "baseline:") {
		t.Errorf("the output does not report the baseline:\n%s", out)
	}
	if len(out) > 2000 {
		t.Errorf("the output is %d bytes for two hits, want it bounded", len(out))
	}
}
