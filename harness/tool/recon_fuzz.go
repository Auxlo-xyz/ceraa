package tool

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/operation"
	"ceraa/harness/scan"
)

// FuzzTool is the model-facing name of the parameter fuzzer. It only shapes
// arguments: the gate, the budget, the receipts and the run itself live in
// harness/scan.
type FuzzTool struct{}

func (f *FuzzTool) Name() string { return "fuzz" }

func (f *FuzzTool) Description() string {
	return "Fuzz one parameter of one URL with a wordlist and report the values that answered differently. Every request is scope-gated, charged to the engagement budget and written a receipt. GET fuzzes the query string, POST fuzzes the body: the body template's {PARAM} is replaced, and a template without the placeholder gets the parameter added as a form field. Give a marker to confirm hits: a value whose response contains the marker is evidence. Without a marker the tool takes a baseline request with an implausible value and reports the values whose status or size differ from it, marked as a delta, which is a difference to chase and not a finding. Returns one line per hit with the status, size, sha256 and receipt id."
}

func (f *FuzzTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":          map[string]any{"type": "string", "description": "Absolute http:// or https:// URL to fuzz. It may already carry a query string"},
			"param":        map[string]any{"type": "string", "description": "Name of the parameter to fuzz"},
			"method":       map[string]any{"type": "string", "enum": []string{"GET", "POST"}, "description": "HTTP method, default GET. GET fuzzes the query, POST fuzzes the body"},
			"location":     map[string]any{"type": "string", "enum": []string{"query", "body"}, "description": "Where the value goes. Empty derives it from the method: query for GET, body for POST"},
			"wordlist":     map[string]any{"type": "string", "description": "Path to a wordlist file of values, one per line, # for comments. Empty uses the embedded list"},
			"marker":       map[string]any{"type": "string", "description": "A substring that marks a hit, matched case insensitively in the response body. Empty means the run reports status and size deltas against a baseline instead"},
			"body":         map[string]any{"type": "string", "description": "Body template for a POST run. {PARAM} is replaced by the fuzz value; without the placeholder the parameter is added as a form field"},
			"concurrency":  map[string]any{"type": "integer", "minimum": 1, "maximum": 32, "description": "Parallel requests, default 8"},
			"max_requests": map[string]any{"type": "integer", "minimum": 1, "maximum": 5000, "description": "Hard ceiling on requests fired, default 200"},
			"timeout":      map[string]any{"type": "integer", "minimum": 1, "maximum": 120, "description": "Per-request timeout in seconds, default 15"},
		},
		"required": []string{"url", "param"},
	}
}

// Translate validates the shape and re-emits the canonical argument map.
func (f *FuzzTool) Translate(args map[string]any) (operation.Spec, error) {
	rawURL := strings.TrimSpace(fuzzStringArg(args["url"]))
	if rawURL == "" {
		return operation.Spec{}, fmt.Errorf("url is required")
	}
	param := strings.TrimSpace(fuzzStringArg(args["param"]))
	if param == "" {
		return operation.Spec{}, fmt.Errorf("param is required")
	}
	method := strings.ToUpper(strings.TrimSpace(fuzzStringArg(args["method"])))
	if method == "" {
		method = "GET"
	}
	if method != "GET" && method != "POST" {
		return operation.Spec{}, fmt.Errorf("method %q is not one of GET, POST", method)
	}
	location := strings.ToLower(strings.TrimSpace(fuzzStringArg(args["location"])))
	if location != "" && location != "query" && location != "body" {
		return operation.Spec{}, fmt.Errorf("location %q is not one of query, body", args["location"])
	}
	out := map[string]any{"url": rawURL, "param": param, "method": method}
	if location != "" {
		out["location"] = location
	}
	if wordlist := strings.TrimSpace(fuzzStringArg(args["wordlist"])); wordlist != "" {
		out["wordlist"] = wordlist
	}
	if marker := strings.TrimSpace(fuzzStringArg(args["marker"])); marker != "" {
		out["marker"] = marker
	}
	if body := fuzzStringArg(args["body"]); body != "" {
		out["body"] = body
	}
	out["concurrency"] = fuzzIntArg(args["concurrency"], 0)
	out["max_requests"] = fuzzIntArg(args["max_requests"], 0)
	out["timeout"] = fuzzIntArg(args["timeout"], 0)
	return operation.Spec{Type: "fuzz", Args: out}, nil
}

func (f *FuzzTool) TranslateResult(op operation.Operation) string { return op.Output }

func (f *FuzzTool) Run(spec operation.Spec) (string, error) {
	return f.RunContext(context.Background(), spec)
}

func (f *FuzzTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	opts := scan.FuzzOptions{
		URL:         fuzzStringArg(spec.Args["url"]),
		Method:      fuzzStringArg(spec.Args["method"]),
		Param:       fuzzStringArg(spec.Args["param"]),
		Location:    fuzzStringArg(spec.Args["location"]),
		Marker:      fuzzStringArg(spec.Args["marker"]),
		Body:        fuzzStringArg(spec.Args["body"]),
		Concurrency: fuzzIntArg(spec.Args["concurrency"], 0),
		MaxRequests: fuzzIntArg(spec.Args["max_requests"], 0),
		Timeout:     fuzzIntArg(spec.Args["timeout"], 0),
	}
	if wordlist := strings.TrimSpace(fuzzStringArg(spec.Args["wordlist"])); wordlist != "" {
		// A relative path from the model means the session workdir, and the
		// resolved path goes in the error so a retry can correct itself.
		base, _ := spec.Args["workdir"].(string)
		resolved, err := ResolvePathFrom(base, wordlist)
		if err != nil {
			return "", fmt.Errorf("wordlist %s: %w", wordlist, err)
		}
		opts.Wordlist = resolved
	}
	result, err := scan.FuzzDetailed(ctx, opts)
	if err != nil {
		return "", err
	}
	return formatFuzzResult(result, opts), nil
}

// formatFuzzResult renders the run: a header, one bounded line per hit and
// the summary of what was fired and what was found.
func formatFuzzResult(result scan.FuzzResult, opts scan.FuzzOptions) string {
	var b strings.Builder
	method := strings.ToUpper(strings.TrimSpace(opts.Method))
	if method == "" {
		method = "GET"
	}
	fmt.Fprintf(&b, "fuzz: %s %s param %s - %d requests fired, %d hits of %d values\n",
		method, opts.URL, opts.Param, result.Requests, len(result.Hits), result.Values)
	for _, hit := range result.Hits {
		kind := "delta"
		if hit.Marker {
			kind = "marker"
		}
		fmt.Fprintf(&b, "%q %s status %d size %d", hit.Value, kind, hit.Status, hit.Size)
		if hit.SHA256 != "" {
			fmt.Fprintf(&b, " sha256:%s", fuzzHash(hit.SHA256))
		}
		if hit.ReceiptID != "" {
			fmt.Fprintf(&b, " receipt:%s", hit.ReceiptID)
		}
		b.WriteString("\n")
	}
	if result.HasBaseline {
		fmt.Fprintf(&b, "baseline: %q status %d size %d - no marker was given, so a hit is a status or size difference, not a confirmed finding\n",
			result.BaselineValue, result.BaselineStatus, result.BaselineSize)
	} else {
		b.WriteString("baseline: none - a marker was given, so hits are confirmed by the marker\n")
	}
	fmt.Fprintf(&b, "refused: %d by scope; errors: %d\n", result.Refused, len(result.Errors))
	if result.Stopped != "" {
		fmt.Fprintf(&b, "stopped: %s\n", result.Stopped)
	}
	for _, message := range result.Errors {
		fmt.Fprintf(&b, "error: %s\n", message)
	}
	return strings.TrimRight(b.String(), "\n")
}

// fuzzStringArg reads a string argument without the zero-value trap.
func fuzzStringArg(v any) string {
	s, _ := v.(string)
	return s
}

// fuzzIntArg reads a non-negative integer argument, falling back when the
// value is absent or unusable.
func fuzzIntArg(v any, fallback int) int {
	switch typed := v.(type) {
	case int:
		if typed >= 0 {
			return typed
		}
	case float64:
		if typed >= 0 && typed == float64(int(typed)) {
			return int(typed)
		}
	}
	return fallback
}

// fuzzHash shortens a digest for a line of output.
func fuzzHash(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}
