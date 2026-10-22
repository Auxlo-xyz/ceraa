package tool

import (
	"strings"
	"testing"

	"ceraa/harness/operation"
	"ceraa/harness/scan"
)

// TestSweepTranslateRequiresModeAndURL: without them there is no operation,
// and the error says so rather than failing later at the sweep.
func TestSweepTranslateRequiresModeAndURL(t *testing.T) {
	tl := &SweepTool{}
	for _, args := range []map[string]any{
		{},
		{"mode": "paths"},
		{"url": "https://example.com"},
		{"mode": "paths", "url": ""},
		{"mode": "paths", "url": 42},
		{"mode": "params", "url": ""},
	} {
		if spec, err := tl.Translate(args); err == nil {
			t.Errorf("Translate(%v) accepted, spec = %v", args, spec)
		}
	}
}

// TestSweepTranslateRequiresTheParamInParamsMode: a schema cannot say that a
// field is required in one mode only, so the tool says it and names the mode.
func TestSweepTranslateRequiresTheParamInParamsMode(t *testing.T) {
	tl := &SweepTool{}
	for _, args := range []map[string]any{
		{"mode": "params", "url": "https://example.com/s"},
		{"mode": "params", "url": "https://example.com/s", "param": ""},
		{"mode": "params", "url": "https://example.com/s", "param": 42},
	} {
		spec, err := tl.Translate(args)
		if err == nil {
			t.Errorf("Translate(%v) accepted, spec = %v", args, spec)
			continue
		}
		if !strings.Contains(err.Error(), "param") {
			t.Errorf("the error does not name what was missing: %v", err)
		}
	}
}

// TestSweepTranslateRefusesAnUnknownMode: the mode chooses the engine, so an
// unknown one has to be refused rather than guessed.
func TestSweepTranslateRefusesAnUnknownMode(t *testing.T) {
	tl := &SweepTool{}
	for _, mode := range []string{"", "sweep", "fuzz", "discover", "burn"} {
		spec, err := tl.Translate(map[string]any{"mode": mode, "url": "https://example.com"})
		if err == nil {
			t.Errorf("mode %q was accepted, spec = %v", mode, spec)
		}
	}
}

// TestSweepTranslateBuildsTheSpec: the operation type has to be the
// registered tool name, because the runner resolves the executor by it.
func TestSweepTranslateBuildsTheSpec(t *testing.T) {
	tl := &SweepTool{}

	spec, err := tl.Translate(map[string]any{"mode": "paths", "url": "https://example.com/"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if spec.Type != "sweep" {
		t.Errorf("spec.Type = %q, want sweep", spec.Type)
	}
	if spec.Args["url"] != "https://example.com/" {
		t.Errorf("spec.Args[url] = %v", spec.Args["url"])
	}
	if spec.Args["mode"] != "paths" {
		t.Errorf("spec.Args[mode] = %v, want paths", spec.Args["mode"])
	}
	if _, ok := spec.Args["paths"]; ok {
		t.Errorf("an absent list was put in the spec: %v", spec.Args)
	}
	if depth, _ := spec.Args["depth"].(int); depth != 0 {
		t.Errorf("spec.Args[depth] = %v, want 0 (unset means the default)", spec.Args["depth"])
	}
}

// TestSweepTranslateReadsEveryArgument: the canonical argument map carries
// what the caller asked for, in the mode that asked for it.
func TestSweepTranslateReadsEveryArgument(t *testing.T) {
	tl := &SweepTool{}

	paths, err := tl.Translate(map[string]any{
		"mode":         "paths",
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
	if list, _ := paths.Args["paths"].([]string); len(list) != 2 || list[0] != "admin" {
		t.Errorf("spec.Args[paths] = %v", paths.Args["paths"])
	}
	if list, _ := paths.Args["extensions"].([]string); len(list) != 2 {
		t.Errorf("spec.Args[extensions] = %v", paths.Args["extensions"])
	}
	if list, _ := paths.Args["match_status"].([]int); len(list) != 2 || list[0] != 200 {
		t.Errorf("spec.Args[match_status] = %v", paths.Args["match_status"])
	}
	if depth, _ := paths.Args["depth"].(int); depth != 2 {
		t.Errorf("spec.Args[depth] = %v", paths.Args["depth"])
	}
	if requests, _ := paths.Args["max_requests"].(int); requests != 50 {
		t.Errorf("spec.Args[max_requests] = %v", paths.Args["max_requests"])
	}
	if results, _ := paths.Args["max_results"].(int); results != 10 {
		t.Errorf("spec.Args[max_results] = %v", paths.Args["max_results"])
	}
	if size, _ := paths.Args["exclude_size"].(int); size != 512 {
		t.Errorf("spec.Args[exclude_size] = %v", paths.Args["exclude_size"])
	}
	if timeout, _ := paths.Args["timeout"].(int); timeout != 5 {
		t.Errorf("spec.Args[timeout] = %v", paths.Args["timeout"])
	}
	if wordlist, _ := paths.Args["wordlist"].(string); wordlist != "/tmp/words.txt" {
		t.Errorf("spec.Args[wordlist] = %v", paths.Args["wordlist"])
	}

	params, err := tl.Translate(map[string]any{
		"mode":         "params",
		"url":          "https://example.com/s",
		"param":        "id",
		"method":       "POST",
		"location":     "body",
		"header":       "X-Forwarded-For",
		"body":         "id={PARAM}",
		"wordlist":     "values.txt",
		"marker":       "sqlstate",
		"concurrency":  float64(4),
		"max_requests": float64(50),
		"timeout":      float64(5),
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if params.Args["param"] != "id" || params.Args["method"] != "POST" || params.Args["location"] != "body" {
		t.Errorf("spec.Args = %v", params.Args)
	}
	if params.Args["marker"] != "sqlstate" || params.Args["body"] != "id={PARAM}" {
		t.Errorf("spec.Args = %v", params.Args)
	}
	if params.Args["header"] != "X-Forwarded-For" {
		t.Errorf("spec.Args[header] = %v", params.Args["header"])
	}
	if requests, _ := params.Args["max_requests"].(int); requests != 50 {
		t.Errorf("spec.Args[max_requests] = %v", params.Args["max_requests"])
	}
	if timeout, _ := params.Args["timeout"].(int); timeout != 5 {
		t.Errorf("spec.Args[timeout] = %v", params.Args["timeout"])
	}
	if wordlist, _ := params.Args["wordlist"].(string); wordlist != "values.txt" {
		t.Errorf("spec.Args[wordlist] = %v", params.Args["wordlist"])
	}
}

// TestSweepModesKeepTheirOwnArguments: the modes are two tools wearing one
// face, so an argument of the other mode must not travel into the spec.
func TestSweepModesKeepTheirOwnArguments(t *testing.T) {
	tl := &SweepTool{}

	paths, err := tl.Translate(map[string]any{
		"mode":   "paths",
		"url":    "https://example.com",
		"param":  "id",
		"method": "POST",
		"marker": "sqlstate",
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	for _, name := range []string{"param", "method", "marker", "location", "header", "body"} {
		if _, ok := paths.Args[name]; ok {
			t.Errorf("the paths spec carries the params argument %q: %v", name, paths.Args)
		}
	}

	params, err := tl.Translate(map[string]any{
		"mode":         "params",
		"url":          "https://example.com/s",
		"param":        "id",
		"paths":        []any{"admin"},
		"extensions":   []any{".php"},
		"depth":        float64(1),
		"max_results":  float64(10),
		"match_status": []any{float64(200)},
		"exclude_size": float64(512),
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	for _, name := range []string{"paths", "extensions", "depth", "max_results", "match_status", "exclude_size"} {
		if _, ok := params.Args[name]; ok {
			t.Errorf("the params spec carries the paths argument %q: %v", name, params.Args)
		}
	}
}

// TestSweepTranslateRefusesWhatItCannotDo: the verb and the location are
// checked in the tool as well as in the package.
func TestSweepTranslateRefusesWhatItCannotDo(t *testing.T) {
	tl := &SweepTool{}
	for _, args := range []map[string]any{
		{"mode": "params", "url": "https://example.com", "param": "id", "method": "DELETE"},
		{"mode": "params", "url": "https://example.com", "param": "id", "method": "PUT"},
		{"mode": "params", "url": "https://example.com", "param": "id", "location": "header"},
		{"mode": "params", "url": "https://example.com", "param": "id", "location": "cookie"},
	} {
		if _, err := tl.Translate(args); err == nil {
			t.Errorf("Translate(%v) accepted", args)
		}
	}
}

// TestSweepName pins the registry name, which is also the operation type.
func TestSweepName(t *testing.T) {
	tl := &SweepTool{}
	if tl.Name() != "sweep" {
		t.Errorf("Name = %q, want sweep", tl.Name())
	}
	spec, err := tl.Translate(map[string]any{"mode": "paths", "url": "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Type != tl.Name() {
		t.Errorf("spec.Type = %q and Name = %q; the runner resolves the executor by the type", spec.Type, tl.Name())
	}
}

// TestTheRegistryCarriesOneSweep: the point of the merge is one catalogue
// entry for one capability, because the catalogue is the cached prefix of
// every request.
func TestTheRegistryCarriesOneSweep(t *testing.T) {
	reg := DefaultRegistry()
	if _, ok := reg.Get("sweep"); !ok {
		t.Fatal("sweep is not registered")
	}
	offered := 0
	for _, tl := range reg.Offered() {
		if tl.Name() == "sweep" {
			offered++
		}
	}
	if offered != 1 {
		t.Errorf("the catalogue carries %d sweep entries, want 1", offered)
	}
	for _, name := range []string{"discover", "fuzz"} {
		if _, ok := reg.Get(name); ok {
			t.Errorf("%q is still registered, so the merge left two entries for one capability", name)
		}
	}
}

// TestSweepDescriptionNamesTheContract: the model reads this, so it has to
// say that the requests are gated and receipted, and that a delta is not a
// finding.
func TestSweepDescriptionNamesTheContract(t *testing.T) {
	description := (&SweepTool{}).Description()
	for _, want := range []string{"scope-gated", "receipt", "delta"} {
		if !strings.Contains(description, want) {
			t.Errorf("the description does not mention %q: %s", want, description)
		}
	}
}

// TestSweepSchemaCoversEveryArgument: a property the tool reads but the
// schema does not declare is refused by the harness before the tool runs.
func TestSweepSchemaCoversEveryArgument(t *testing.T) {
	tl := &SweepTool{}
	schema := tl.Schema()
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range []string{
		// shared
		"mode", "url", "wordlist", "concurrency", "max_requests", "timeout",
		// paths mode
		"paths", "extensions", "depth", "max_results", "match_status", "exclude_size",
		// params mode
		"param", "method", "location", "header", "body", "marker",
	} {
		if _, ok := properties[name]; !ok {
			t.Errorf("the schema does not declare %q", name)
		}
	}
	required, _ := schema["required"].([]string)
	if len(required) != 2 || required[0] != "mode" || required[1] != "url" {
		t.Errorf("required = %v, want mode and url", required)
	}
	// The harness validates the model's arguments against this schema
	// before the tool ever sees them.
	for _, args := range []map[string]any{
		{
			"mode":         "paths",
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
		},
		{
			"mode":         "params",
			"url":          "https://example.com/s",
			"param":        "id",
			"method":       "POST",
			"location":     "body",
			"header":       "X-Forwarded-For",
			"wordlist":     "values.txt",
			"marker":       "sqlstate",
			"body":         "id={PARAM}",
			"concurrency":  float64(4),
			"max_requests": float64(50),
			"timeout":      float64(5),
		},
	} {
		if err := ValidateArguments(schema, args); err != nil {
			t.Errorf("the harness refuses the declared arguments: %v", err)
		}
	}
	// An argument the tool does not read is still refused: the merged schema
	// is wider than either mode, and a wider schema is not a looser one.
	if err := ValidateArguments(schema, map[string]any{
		"mode":  "paths",
		"url":   "https://example.com",
		"probe": "everything",
	}); err == nil {
		t.Error("the harness accepted an argument the tool does not read")
	}
}

// TestSweepTranslateResultPassesOutput: result translation is a pass
// through of the recorded output.
func TestSweepTranslateResultPassesOutput(t *testing.T) {
	tl := &SweepTool{}
	op := operation.Operation{Output: "discover: https://example.com - 1 requests fired, 0 hits"}
	if got := tl.TranslateResult(op); got != op.Output {
		t.Errorf("TranslateResult = %q, want passthrough", got)
	}
}

// TestSweepRunContextRefusesAnUnknownMode: the dispatch has to name the mode
// it does not know, because a sweep fires requests.
func TestSweepRunContextRefusesAnUnknownMode(t *testing.T) {
	tl := &SweepTool{}
	if _, err := tl.RunContext(t.Context(), operation.Spec{Type: "sweep", Args: map[string]any{"mode": "burn"}}); err == nil {
		t.Error("RunContext accepted an unknown mode")
	}
	if _, err := tl.RunContext(t.Context(), operation.Spec{Type: "sweep", Args: map[string]any{}}); err == nil {
		t.Error("RunContext accepted a spec with no mode")
	}
}

// TestSweepWordlistResolvesAgainstTheSessionWorkdir: a relative path from the
// model means where its commands run, not the process directory.
func TestSweepWordlistResolvesAgainstTheSessionWorkdir(t *testing.T) {
	spec := operation.Spec{Type: "sweep", Args: map[string]any{"workdir": "/tmp/session"}}
	resolved, err := sweepWordlist(spec, "words.txt")
	if err != nil {
		t.Fatalf("sweepWordlist: %v", err)
	}
	if resolved != "/tmp/session/words.txt" {
		t.Errorf("resolved = %q, want it under the session workdir", resolved)
	}
	empty, err := sweepWordlist(spec, "  ")
	if err != nil {
		t.Fatalf("sweepWordlist: %v", err)
	}
	if empty != "" {
		t.Errorf("resolved = %q, want the embedded list to stay in charge", empty)
	}
}

// TestFormatSweepPathsResultIsBounded: the summary says what was fired, what
// was found and what was filtered, and it never prints a body.
func TestFormatSweepPathsResultIsBounded(t *testing.T) {
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
	out := formatSweepPathsResult(result, "https://example.com")
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

// TestFormatSweepParamsResultSaysADeltaIsNotAFinding: the difference between a
// marker hit and a delta has to survive into the output.
func TestFormatSweepParamsResultSaysADeltaIsNotAFinding(t *testing.T) {
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
	out := formatSweepParamsResult(result, scan.FuzzOptions{URL: "https://example.com/s", Param: "id"})
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
