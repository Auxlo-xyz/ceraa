package tool

// recon_sweep.go is the wordlist sweep wearing a tool face.
//
// It is one tool because it is one capability: a wordlist against a target.
// The two shapes it took - a walk of the site's paths and a mutation of one
// parameter - are the same engine pointed at two things, and the catalogue is
// the cached prefix of every request, so two entries for one capability is two
// entries the model reads on every turn.
//
// The engines are untouched. harness/scan still owns the gate, the budget, the
// receipts and the sweep, and this file only shapes arguments and dispatches
// on the mode. Each mode keeps its own output, its own summary and its own
// bounds, because a path hit and a parameter delta are not the same fact and
// a merged rendering would blur the difference between them.
//
// The mode decides which arguments apply, and a schema cannot say that: JSON
// schema can make a field required but not required in one mode only. So the
// per-mode requirements are enforced here, where the error can name the mode
// that was missing the argument.

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/operation"
	"ceraa/harness/scan"
)

// SweepTool is the model-facing name of the wordlist sweep.
type SweepTool struct{}

// The executor resolves a tool by its operation type and calls it through the
// context runner, so the two have to agree with the name.
var _ operation.ContextRunner = (*SweepTool)(nil)

func (s *SweepTool) Name() string { return "sweep" }

func (s *SweepTool) Description() string {
	return "Sweep a target with a wordlist: mode paths walks site paths for content discovery, mode params mutates one parameter and reports marker hits or size and status deltas. Every request is scope-gated and receipted."
}

func (s *SweepTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			// The shared arguments, stated once.
			"mode":         map[string]any{"type": "string", "enum": []string{"paths", "params"}, "description": "paths to sweep site paths, params to fuzz one parameter"},
			"url":          map[string]any{"type": "string", "description": "The base site URL in paths mode, the full endpoint URL in params mode"},
			"wordlist":     map[string]any{"type": "string", "description": "Path to a wordlist of paths or of values"},
			"concurrency":  map[string]any{"type": "integer", "minimum": 1, "maximum": 32, "description": "Parallel requests, default 8"},
			"max_requests": map[string]any{"type": "integer", "minimum": 1, "maximum": 5000, "description": "Request ceiling, default 200"},
			"timeout":      map[string]any{"type": "integer", "minimum": 1, "maximum": 120, "description": "Timeout in seconds, default 15"},

			// The paths mode arguments.
			"paths":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 100, "description": "paths mode: relative paths to try"},
			"extensions":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 16, "description": "paths mode: suffixes appended to each word"},
			"depth":        map[string]any{"type": "integer", "minimum": 0, "maximum": 3, "description": "paths mode: recursion depth, default 0"},
			"max_results":  map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "description": "paths mode: hit ceiling, default 100"},
			"match_status": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "maxItems": 32, "description": "paths mode: status codes to keep"},
			"exclude_size": map[string]any{"type": "integer", "minimum": 0, "description": "paths mode: drop responses of exactly this size"},

			// The params mode arguments.
			"param":    map[string]any{"type": "string", "description": "params mode: the parameter to fuzz"},
			"method":   map[string]any{"type": "string", "enum": []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}, "description": "params mode: GET, POST, PUT, PATCH, DELETE, HEAD or OPTIONS"},
			"location": map[string]any{"type": "string", "enum": []string{"query", "body", "header"}, "description": "params mode: query, body or header"},
			"header":   map[string]any{"type": "string", "description": "params mode: the header to fuzz"},
			"body":     map[string]any{"type": "string", "description": "params mode: body template with {PARAM}"},
			"marker":   map[string]any{"type": "string", "description": "params mode: text that marks a hit"},
		},
		// The mode and the url are required by every mode. The rest are
		// required by one mode or the other, and the schema cannot say which,
		// so the tool enforces those per mode.
		"required": []string{"mode", "url"},
	}
}

// Translate validates the shape and re-emits the canonical argument map.
func (s *SweepTool) Translate(args map[string]any) (operation.Spec, error) {
	mode, err := sweepMode(args["mode"])
	if err != nil {
		return operation.Spec{}, err
	}
	out := map[string]any{"mode": mode}

	switch mode {
	case "paths":
		rawURL := strings.TrimSpace(sweepStringArg(args["url"]))
		if rawURL == "" {
			return operation.Spec{}, fmt.Errorf("mode paths needs the base url to sweep")
		}
		out["url"] = rawURL
		if paths := sweepStringListArg(args["paths"], 100); len(paths) > 0 {
			out["paths"] = paths
		}
		if extensions := sweepStringListArg(args["extensions"], 16); len(extensions) > 0 {
			out["extensions"] = extensions
		}
		if statuses := sweepIntListArg(args["match_status"], 32); len(statuses) > 0 {
			out["match_status"] = statuses
		}
		out["depth"] = sweepIntArg(args["depth"], 0)
		out["max_results"] = sweepIntArg(args["max_results"], 0)
		out["exclude_size"] = sweepIntArg(args["exclude_size"], 0)

	case "params":
		rawURL := strings.TrimSpace(sweepStringArg(args["url"]))
		if rawURL == "" {
			return operation.Spec{}, fmt.Errorf("mode params needs the url to fuzz")
		}
		param := strings.TrimSpace(sweepStringArg(args["param"]))
		if param == "" {
			return operation.Spec{}, fmt.Errorf("mode params needs the param to fuzz")
		}
		method := strings.ToUpper(strings.TrimSpace(sweepStringArg(args["method"])))
		if method == "" {
			method = "GET"
		}
		if method != "GET" && method != "POST" {
			return operation.Spec{}, fmt.Errorf("method %q is not one of GET, POST", method)
		}
		location := strings.ToLower(strings.TrimSpace(sweepStringArg(args["location"])))
		if location != "" && location != "query" && location != "body" && location != "header" {
			return operation.Spec{}, fmt.Errorf("location %q is not one of query, body, header", args["location"])
		}
		header := strings.TrimSpace(sweepStringArg(args["header"]))
		if location == "header" && header == "" {
			return operation.Spec{}, fmt.Errorf("location header needs a header name")
		}
		out["url"] = rawURL
		out["param"] = param
		out["method"] = method
		if header != "" {
			out["header"] = header
		}
		if location != "" {
			out["location"] = location
		}
		if marker := strings.TrimSpace(sweepStringArg(args["marker"])); marker != "" {
			out["marker"] = marker
		}
		if body := sweepStringArg(args["body"]); body != "" {
			out["body"] = body
		}
	}

	// The shared arguments: one wordlist, one concurrency, one ceiling and one
	// timeout, whichever mode is running.
	if wordlist := strings.TrimSpace(sweepStringArg(args["wordlist"])); wordlist != "" {
		out["wordlist"] = wordlist
	}
	out["concurrency"] = sweepIntArg(args["concurrency"], 0)
	out["max_requests"] = sweepIntArg(args["max_requests"], 0)
	out["timeout"] = sweepIntArg(args["timeout"], 0)

	return operation.Spec{Type: "sweep", Args: out}, nil
}

func (s *SweepTool) TranslateResult(op operation.Operation) string { return op.Output }

func (s *SweepTool) Run(spec operation.Spec) (string, error) {
	return s.RunContext(context.Background(), spec)
}

// RunContext dispatches on the mode.
//
// The mode is the only thing that decides which engine runs. Everything the
// engine needs came out of Translate, so the two halves of the tool cannot
// drift apart: a mode that gets a missing argument is refused at the
// translation, not half-way through a sweep.
func (s *SweepTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(sweepStringArg(spec.Args["mode"])))
	switch mode {
	case "paths":
		return s.sweepPaths(ctx, spec)
	case "params":
		return s.sweepParams(ctx, spec)
	}
	return "", fmt.Errorf("mode %q is not one of paths, params", mode)
}

// sweepPaths walks the paths of a site with a wordlist.
func (s *SweepTool) sweepPaths(ctx context.Context, spec operation.Spec) (string, error) {
	opts := scan.DiscoverOptions{
		BaseURL:     sweepStringArg(spec.Args["url"]),
		Paths:       sweepStringListArg(spec.Args["paths"], 100),
		Extensions:  sweepStringListArg(spec.Args["extensions"], 16),
		MatchStatus: sweepIntListArg(spec.Args["match_status"], 32),
		Depth:       sweepIntArg(spec.Args["depth"], 0),
		Concurrency: sweepIntArg(spec.Args["concurrency"], 0),
		MaxRequests: sweepIntArg(spec.Args["max_requests"], 0),
		MaxResults:  sweepIntArg(spec.Args["max_results"], 0),
		ExcludeSize: sweepIntArg(spec.Args["exclude_size"], 0),
		Timeout:     sweepIntArg(spec.Args["timeout"], 0),
	}
	resolved, err := sweepWordlist(spec, sweepStringArg(spec.Args["wordlist"]))
	if err != nil {
		return "", err
	}
	opts.Wordlist = resolved
	result, err := scan.DiscoverDetailed(ctx, opts)
	if err != nil {
		return "", err
	}
	return formatSweepPathsResult(result, opts.BaseURL), nil
}

// sweepParams mutates one parameter with a wordlist of values.
func (s *SweepTool) sweepParams(ctx context.Context, spec operation.Spec) (string, error) {
	opts := scan.FuzzOptions{
		URL:         sweepStringArg(spec.Args["url"]),
		Method:      sweepStringArg(spec.Args["method"]),
		Param:       sweepStringArg(spec.Args["param"]),
		Location:    sweepStringArg(spec.Args["location"]),
		Header:      sweepStringArg(spec.Args["header"]),
		Marker:      sweepStringArg(spec.Args["marker"]),
		Body:        sweepStringArg(spec.Args["body"]),
		Concurrency: sweepIntArg(spec.Args["concurrency"], 0),
		MaxRequests: sweepIntArg(spec.Args["max_requests"], 0),
		Timeout:     sweepIntArg(spec.Args["timeout"], 0),
	}
	resolved, err := sweepWordlist(spec, sweepStringArg(spec.Args["wordlist"]))
	if err != nil {
		return "", err
	}
	opts.Wordlist = resolved
	result, err := scan.FuzzDetailed(ctx, opts)
	if err != nil {
		return "", err
	}
	return formatSweepParamsResult(result, opts), nil
}

// sweepWordlist resolves the wordlist path against the session workdir.
//
// A relative path from the model means the session workdir, not the process
// directory, and the resolved path goes in the error so a retry can correct
// itself. An empty wordlist keeps the embedded list, which is the engine's
// default.
func sweepWordlist(spec operation.Spec, wordlist string) (string, error) {
	trimmed := strings.TrimSpace(wordlist)
	if trimmed == "" {
		return "", nil
	}
	base, _ := spec.Args["workdir"].(string)
	resolved, err := ResolvePathFrom(base, trimmed)
	if err != nil {
		return "", fmt.Errorf("wordlist %s: %w", trimmed, err)
	}
	return resolved, nil
}

// sweepMode reads the mode argument.
//
// There is no default. A sweep fires up to five thousand requests, and the
// two modes ask for different things: guessing would run the wrong one.
func sweepMode(value any) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(sweepStringArg(value)))
	switch mode {
	case "paths", "params":
		return mode, nil
	}
	return "", fmt.Errorf("mode %q is not one of paths, params", mode)
}

// formatSweepPathsResult renders the path sweep: a header, one bounded line
// per hit, and the summary of what was fired, found and filtered.
func formatSweepPathsResult(result scan.DiscoverResult, base string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "sweep paths: %s - %d requests fired, %d hits\n", base, result.Requests, len(result.Hits))
	for _, hit := range result.Hits {
		fmt.Fprintf(&b, "%s %d %s", hit.Path, hit.Status, sweepSize(hit.Size))
		if hit.ContentType != "" {
			fmt.Fprintf(&b, " %s", hit.ContentType)
		}
		if hit.SHA256 != "" {
			fmt.Fprintf(&b, " sha256:%s", sweepHash(hit.SHA256))
		}
		if hit.ReceiptID != "" {
			fmt.Fprintf(&b, " receipt:%s", hit.ReceiptID)
		}
		fmt.Fprintf(&b, " %dms\n", hit.ElapsedMS)
	}
	if result.Collapsed != nil {
		fmt.Fprintf(&b, "catch-all: %d paths answered the same %d response of %d bytes (folded into one line); sample: %s\n",
			result.Collapsed.Paths, result.Collapsed.Status, result.Collapsed.Size, strings.Join(result.Collapsed.Sample, ", "))
	}
	fmt.Fprintf(&b, "excluded: %d soft-404 (%s); %d by status; %d by explicit size; %d over the result cap\n",
		result.ExcludedSoft404, sweepSizes(result.Soft404Sizes), result.ExcludedStatus, result.ExcludedSize, result.ExcludedCap)
	fmt.Fprintf(&b, "refused: %d by scope; errors: %d\n", result.Refused, len(result.Errors))
	if result.Stopped != "" {
		fmt.Fprintf(&b, "stopped: %s\n", result.Stopped)
	}
	for _, message := range result.Errors {
		fmt.Fprintf(&b, "error: %s\n", message)
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatSweepParamsResult renders the parameter run: a header, one bounded
// line per hit and the summary of what was fired and what was found.
func formatSweepParamsResult(result scan.FuzzResult, opts scan.FuzzOptions) string {
	var b strings.Builder
	method := strings.ToUpper(strings.TrimSpace(opts.Method))
	if method == "" {
		method = "GET"
	}
	fmt.Fprintf(&b, "sweep params: %s %s param %s - %d requests fired, %d hits of %d values\n",
		method, opts.URL, opts.Param, result.Requests, len(result.Hits), result.Values)
	for _, hit := range result.Hits {
		kind := "delta"
		if hit.Marker {
			kind = "marker"
		}
		fmt.Fprintf(&b, "%q %s status %d size %d", hit.Value, kind, hit.Status, hit.Size)
		if hit.SHA256 != "" {
			fmt.Fprintf(&b, " sha256:%s", sweepHash(hit.SHA256))
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

// sweepStringArg reads a string argument without the zero-value trap.
func sweepStringArg(v any) string {
	s, _ := v.(string)
	return s
}

// sweepIntArg reads a non-negative integer argument, falling back when the
// value is absent or unusable.
func sweepIntArg(v any, fallback int) int {
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

// sweepStringListArg reads a list of strings in both the native and the
// JSON-decoded shape.
func sweepStringListArg(v any, max int) []string {
	switch typed := v.(type) {
	case []string:
		if len(typed) > max {
			typed = typed[:max]
		}
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			if s, ok := entry.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		if len(out) > max {
			out = out[:max]
		}
		return out
	}
	return nil
}

// sweepIntListArg reads a list of integers in both shapes.
func sweepIntListArg(v any, max int) []int {
	switch typed := v.(type) {
	case []int:
		if len(typed) > max {
			typed = typed[:max]
		}
		return typed
	case []any:
		out := make([]int, 0, len(typed))
		for _, entry := range typed {
			switch number := entry.(type) {
			case int:
				out = append(out, number)
			case float64:
				if number == float64(int(number)) {
					out = append(out, int(number))
				}
			}
		}
		if len(out) > max {
			out = out[:max]
		}
		return out
	}
	return nil
}

// sweepSize renders a body size in a short human form.
func sweepSize(size int) string {
	switch {
	case size < 1024:
		return fmt.Sprintf("%dB", size)
	case size < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(size)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(size)/(1024*1024))
	}
}

// sweepHash shortens a digest for a line of output.
func sweepHash(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}

// sweepSizes renders a list of sizes for the summary.
func sweepSizes(sizes []int) string {
	if len(sizes) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(sizes))
	for _, size := range sizes {
		parts = append(parts, sweepSize(size))
	}
	return strings.Join(parts, ", ")
}

// The names this tool had before the merge.
//
// The registry still registers both and the registry is not this file's to
