package tool

import (
	"strings"
	"testing"

	"ceraa/harness/operation"
	"ceraa/harness/scan"
)

// TestDiscoverTranslateRequiresURL: without a url there is no operation, and
// the error says so rather than failing later at the sweep.
func TestDiscoverTranslateRequiresURL(t *testing.T) {
	tl := &DiscoverTool{}
	for _, args := range []map[string]any{
		{},
		{"url": ""},
		{"url": 42},
	} {
		if spec, err := tl.Translate(args); err == nil {
			t.Errorf("Translate(%v) accepted, spec = %v", args, spec)
		}
	}
}

// TestDiscoverTranslateBuildsTheSpec: the operation type has to be the
// registered tool name, because the runner resolves the executor by it.
func TestDiscoverTranslateBuildsTheSpec(t *testing.T) {
	tl := &DiscoverTool{}

	spec, err := tl.Translate(map[string]any{"url": "https://example.com/"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if spec.Type != "discover" {
		t.Errorf("spec.Type = %q, want discover", spec.Type)
	}
	if spec.Args["url"] != "https://example.com/" {
		t.Errorf("spec.Args[url] = %v", spec.Args["url"])
	}
	if _, ok := spec.Args["paths"]; ok {
		t.Errorf("an absent list was put in the spec: %v", spec.Args)
	}
	if depth, _ := spec.Args["depth"].(int); depth != 0 {
		t.Errorf("spec.Args[depth] = %v, want 0 (unset means the default)", spec.Args["depth"])
	}
}

// TestDiscoverTranslateReadsEveryArgument: the canonical argument map
// carries what the caller asked for.
func TestDiscoverTranslateReadsEveryArgument(t *testing.T) {
	tl := &DiscoverTool{}
	spec, err := tl.Translate(map[string]any{
		"url":          "https://example.com",
		"paths":        []any{"admin", "/backup"},
		"wordlist":     "/tmp/words.txt",
		"extensions":   []any{".php", ".bak"},
		"depth":        float64(2),
		"concurrency":  float64(4),
		"max_requests": float64(50),
		"max_results":  float64(10),
		"match_status": []any{float64(200), float64(301)},
		"exclude_size": float64(512),
		"timeout":      float64(5),
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if paths, _ := spec.Args["paths"].([]string); len(paths) != 2 || paths[0] != "admin" {
		t.Errorf("spec.Args[paths] = %v", spec.Args["paths"])
	}
	if extensions, _ := spec.Args["extensions"].([]string); len(extensions) != 2 {
		t.Errorf("spec.Args[extensions] = %v", spec.Args["extensions"])
	}
	if statuses, _ := spec.Args["match_status"].([]int); len(statuses) != 2 || statuses[0] != 200 {
		t.Errorf("spec.Args[match_status] = %v", spec.Args["match_status"])
	}
	if depth, _ := spec.Args["depth"].(int); depth != 2 {
		t.Errorf("spec.Args[depth] = %v", spec.Args["depth"])
	}
	if requests, _ := spec.Args["max_requests"].(int); requests != 50 {
		t.Errorf("spec.Args[max_requests] = %v", spec.Args["max_requests"])
	}
	if size, _ := spec.Args["exclude_size"].(int); size != 512 {
		t.Errorf("spec.Args[exclude_size] = %v", spec.Args["exclude_size"])
	}
	if timeout, _ := spec.Args["timeout"].(int); timeout != 5 {
		t.Errorf("spec.Args[timeout] = %v", spec.Args["timeout"])
	}
	if wordlist, _ := spec.Args["wordlist"].(string); wordlist != "/tmp/words.txt" {
		t.Errorf("spec.Args[wordlist] = %v", spec.Args["wordlist"])
	}
}

// TestDiscoverName pins the registry name, which is also the operation type.
func TestDiscoverName(t *testing.T) {
	tl := &DiscoverTool{}
	if tl.Name() != "discover" {
		t.Errorf("Name = %q, want discover", tl.Name())
	}
	spec, err := tl.Translate(map[string]any{"url": "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Type != tl.Name() {
		t.Errorf("spec.Type = %q and Name = %q; the runner resolves the executor by the type", spec.Type, tl.Name())
	}
}

// TestDiscoverDescriptionNamesTheContract: the model reads this, so it has
// to say that the requests are gated and receipted.
func TestDiscoverDescriptionNamesTheContract(t *testing.T) {
	description := (&DiscoverTool{}).Description()
	for _, want := range []string{"scope-gated", "receipt"} {
		if !strings.Contains(description, want) {
			t.Errorf("the description does not mention %q: %s", want, description)
		}
	}
}

// TestDiscoverSchemaCoversTheArguments: a property the tool reads but the
// schema does not declare is refused by the harness before the tool runs.
func TestDiscoverSchemaCoversTheArguments(t *testing.T) {
	tl := &DiscoverTool{}
	schema := tl.Schema()
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range []string{
		"url", "paths", "wordlist", "extensions", "depth", "concurrency",
		"max_requests", "max_results", "match_status", "exclude_size", "timeout",
	} {
		if _, ok := properties[name]; !ok {
			t.Errorf("the schema does not declare %q", name)
		}
	}
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "url" {
		t.Errorf("required = %v, want [url]", required)
	}
	// The harness validates the model's arguments against this schema
	// before the tool ever sees them.
	args := map[string]any{
		"url":          "https://example.com",
		"paths":        []any{"admin"},
		"wordlist":     "words.txt",
		"extensions":   []any{".php"},
		"depth":        float64(1),
		"concurrency":  float64(4),
		"max_requests": float64(50),
		"max_results":  float64(10),
		"match_status": []any{float64(200)},
		"exclude_size": float64(512),
		"timeout":      float64(5),
	}
	if err := ValidateArguments(schema, args); err != nil {
		t.Errorf("the harness refuses the declared arguments: %v", err)
	}
}

// TestDiscoverTranslateResultPassesOutput: result translation is a pass
// through of the recorded output.
func TestDiscoverTranslateResultPassesOutput(t *testing.T) {
	tl := &DiscoverTool{}
	op := operation.Operation{Output: "discover: https://example.com - 1 requests fired, 0 hits"}
	if got := tl.TranslateResult(op); got != op.Output {
		t.Errorf("TranslateResult = %q, want passthrough", got)
	}
}

// TestFormatDiscoverResultIsBounded: the summary says what was fired, what
// was found and what was filtered, and it never prints a body.
func TestFormatDiscoverResultIsBounded(t *testing.T) {
	result := scan.DiscoverResult{
		Hits: []scan.Hit{{
			Path:        "/admin",
			Status:      200,
			Size:        1024,
			ContentType: "text/html",
			SHA256:      "0123456789abcdef0123456789abcdef",
			ReceiptID:   "20260101T000000.000000000Z-0123456789ab.json",
			ElapsedMS:   12,
		}},
		Requests:        42,
		ExcludedSoft404: 7,
		Soft404Sizes:    []int{512},
	}
	out := formatDiscoverResult(result, "https://example.com")
	for _, want := range []string{"requests fired", "hits", "excluded:", "refused:"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not say %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "/admin") {
		t.Errorf("the hit is not in the output:\n%s", out)
	}
	if !strings.Contains(out, "512B") {
		t.Errorf("the soft-404 size is not named:\n%s", out)
	}
	if len(out) > 2000 {
		t.Errorf("the output is %d bytes for one hit, want it bounded", len(out))
	}
}
