package tool

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/operation"
	"ceraa/harness/scan"
)

// DiscoverTool is the model-facing name of the content-discovery sweep. It
// only shapes arguments: the gate, the budget, the receipts and the sweep
// itself live in harness/scan, and registration happens in the registry, not
// here.
type DiscoverTool struct{}

func (d *DiscoverTool) Name() string { return "discover" }

func (d *DiscoverTool) Description() string {
	return "Sweep a site for content: explicit paths plus a wordlist of admin panels, API paths, config files, backup files and version control leftovers, each one a bounded GET. Every request is scope-gated, charged to the engagement budget and written a receipt, so the sweep leaves the same evidence trail a single probe does. Returns one line per hit (path, status, size, content type, sha256, receipt id) and a summary of what was fired, what was found and what was filtered out as a soft 404. "
}

func (d *DiscoverTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":          map[string]any{"type": "string", "description": "Absolute http:// or https:// base URL to sweep"},
			"paths":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 100, "description": "Explicit relative paths to try as given, on top of the wordlist"},
			"wordlist":     map[string]any{"type": "string", "description": "Path to a wordlist file, one entry per line, # for comments. Empty uses the embedded list of admin panels, API paths, config, backup and version control artefacts"},
			"extensions":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 16, "description": "Suffixes appended to each wordlist word, for example .php, .bak, .zip"},
			"depth":        map[string]any{"type": "integer", "minimum": 0, "maximum": 3, "description": "Directory recursion depth for directories the sweep finds, 0 disables it"},
			"concurrency":  map[string]any{"type": "integer", "minimum": 1, "maximum": 32, "description": "Parallel requests, default 8"},
			"max_requests": map[string]any{"type": "integer", "minimum": 1, "maximum": 5000, "description": "Hard ceiling on requests fired, default 200"},
			"max_results":  map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "description": "Hard ceiling on hits returned, default 100"},
			"match_status": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "maxItems": 32, "description": "Status codes to keep, empty keeps every status"},
			"exclude_size": map[string]any{"type": "integer", "minimum": 0, "description": "Drop responses of exactly this size, the explicit form of the soft-404 rule"},
			"timeout":      map[string]any{"type": "integer", "minimum": 1, "maximum": 120, "description": "Per-request timeout in seconds, default 15"},
		},
		"required": []string{"url"},
	}
}

// Translate validates the shape and re-emits the canonical argument map.
func (d *DiscoverTool) Translate(args map[string]any) (operation.Spec, error) {
	rawURL := strings.TrimSpace(discoverStringArg(args["url"]))
	if rawURL == "" {
		return operation.Spec{}, fmt.Errorf("url is required")
	}
	out := map[string]any{"url": rawURL}
	if paths := discoverStringListArg(args["paths"], 100); len(paths) > 0 {
		out["paths"] = paths
	}
	if wordlist := strings.TrimSpace(discoverStringArg(args["wordlist"])); wordlist != "" {
		out["wordlist"] = wordlist
	}
	if extensions := discoverStringListArg(args["extensions"], 16); len(extensions) > 0 {
		out["extensions"] = extensions
	}
	if statuses := discoverIntListArg(args["match_status"], 32); len(statuses) > 0 {
		out["match_status"] = statuses
	}
	out["depth"] = discoverIntArg(args["depth"], 0)
	out["concurrency"] = discoverIntArg(args["concurrency"], 0)
	out["max_requests"] = discoverIntArg(args["max_requests"], 0)
	out["max_results"] = discoverIntArg(args["max_results"], 0)
	out["exclude_size"] = discoverIntArg(args["exclude_size"], 0)
	out["timeout"] = discoverIntArg(args["timeout"], 0)
	return operation.Spec{Type: "discover", Args: out}, nil
}

func (d *DiscoverTool) TranslateResult(op operation.Operation) string { return op.Output }

func (d *DiscoverTool) Run(spec operation.Spec) (string, error) {
	return d.RunContext(context.Background(), spec)
}

func (d *DiscoverTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	opts := scan.DiscoverOptions{
		BaseURL:     discoverStringArg(spec.Args["url"]),
		Paths:       discoverStringListArg(spec.Args["paths"], 100),
		Extensions:  discoverStringListArg(spec.Args["extensions"], 16),
		MatchStatus: discoverIntListArg(spec.Args["match_status"], 32),
		Depth:       discoverIntArg(spec.Args["depth"], 0),
		Concurrency: discoverIntArg(spec.Args["concurrency"], 0),
		MaxRequests: discoverIntArg(spec.Args["max_requests"], 0),
		MaxResults:  discoverIntArg(spec.Args["max_results"], 0),
		ExcludeSize: discoverIntArg(spec.Args["exclude_size"], 0),
		Timeout:     discoverIntArg(spec.Args["timeout"], 0),
	}
	if wordlist := strings.TrimSpace(discoverStringArg(spec.Args["wordlist"])); wordlist != "" {
		// A relative path from the model means the session workdir, not the
		// process directory, and the resolved path goes in the error so a
		// retry can correct itself.
		base, _ := spec.Args["workdir"].(string)
		resolved, err := ResolvePathFrom(base, wordlist)
		if err != nil {
			return "", fmt.Errorf("wordlist %s: %w", wordlist, err)
		}
		opts.Wordlist = resolved
	}
	result, err := scan.DiscoverDetailed(ctx, opts)
	if err != nil {
		return "", err
	}
	return formatDiscoverResult(result, opts.BaseURL), nil
}

// formatDiscoverResult renders the sweep: a header, one bounded line per
// hit, and the summary of what was fired, found and filtered.
func formatDiscoverResult(result scan.DiscoverResult, base string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "discover: %s - %d requests fired, %d hits\n", base, result.Requests, len(result.Hits))
	for _, hit := range result.Hits {
		fmt.Fprintf(&b, "%s %d %s", hit.Path, hit.Status, discoverSize(hit.Size))
		if hit.ContentType != "" {
			fmt.Fprintf(&b, " %s", hit.ContentType)
		}
		if hit.SHA256 != "" {
			fmt.Fprintf(&b, " sha256:%s", discoverHash(hit.SHA256))
		}
		if hit.ReceiptID != "" {
			fmt.Fprintf(&b, " receipt:%s", hit.ReceiptID)
		}
		fmt.Fprintf(&b, " %dms\n", hit.ElapsedMS)
	}
	fmt.Fprintf(&b, "excluded: %d soft-404 (%s); %d by status; %d by explicit size; %d over the result cap\n",
		result.ExcludedSoft404, discoverSizes(result.Soft404Sizes), result.ExcludedStatus, result.ExcludedSize, result.ExcludedCap)
	fmt.Fprintf(&b, "refused: %d by scope; errors: %d\n", result.Refused, len(result.Errors))
	if result.Stopped != "" {
		fmt.Fprintf(&b, "stopped: %s\n", result.Stopped)
	}
	for _, message := range result.Errors {
		fmt.Fprintf(&b, "error: %s\n", message)
	}
	return strings.TrimRight(b.String(), "\n")
}

// discoverStringArg reads a string argument without the zero-value trap.
func discoverStringArg(v any) string {
	s, _ := v.(string)
	return s
}

// discoverIntArg reads a non-negative integer argument, falling back when the
// value is absent or unusable.
func discoverIntArg(v any, fallback int) int {
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

// discoverStringListArg reads a list of strings in both the native and the
// JSON-decoded shape.
func discoverStringListArg(v any, max int) []string {
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

// discoverIntListArg reads a list of integers in both shapes.
func discoverIntListArg(v any, max int) []int {
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

// discoverSize renders a body size in a short human form.
func discoverSize(size int) string {
	switch {
	case size < 1024:
		return fmt.Sprintf("%dB", size)
	case size < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(size)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(size)/(1024*1024))
	}
}

// discoverHash shortens a digest for a line of output.
func discoverHash(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}

// discoverSizes renders a list of sizes for the summary.
func discoverSizes(sizes []int) string {
	if len(sizes) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(sizes))
	for _, size := range sizes {
		parts = append(parts, discoverSize(size))
	}
	return strings.Join(parts, ", ")
}
