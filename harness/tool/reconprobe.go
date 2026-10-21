package tool

import (
	"context"
	"fmt"

	"ceraa/harness/operation"
	"ceraa/harness/recon"
)

// ProbeTool is the model-facing name of the recon probe. It only shapes
// arguments: scope gating, the rate budget and execution live in the recon
// package, and registration happens in the registry, not here.
type ProbeTool struct{}

func (p *ProbeTool) Name() string { return "probe" }

func (p *ProbeTool) Description() string {
	return "Send an HTTP probe and get per-request observations: status, elapsed, header subset, redirect, body hash and a receipt file. Modes: single (one request), path_list (relative paths appended to url, max 30), origin_variants (Origin header per value plus Origin null, with CORS reflection verdict). Every request is scope-gated and metered against the engagement rate budget."
}

func (p *ProbeTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":     map[string]any{"type": "string", "description": "Absolute http:// or https:// URL to probe"},
			"mode":    map[string]any{"type": "string", "enum": []string{"single", "path_list", "origin_variants"}, "description": "single (default) fires one request; path_list appends each relative path to url as its own request; origin_variants adds Origin-header variants plus the null origin"},
			"paths":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 30, "description": "Relative paths for path_list mode, maximum 30"},
			"origins": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 12, "description": "Origin values for origin_variants mode, maximum 12"},
			"headers": map[string]any{"type": "object", "description": "Request headers as string name/value pairs"},
			"body":    map[string]any{"type": "string", "description": "Request body, capped at 64k"},
			"timeout": map[string]any{"type": "integer", "description": "Per-request timeout in seconds, default 15, maximum 120"},
		},
		"required": []string{"url"},
	}
}

// Translate validates the shape and re-emits the canonical argument map.
func (p *ProbeTool) Translate(args map[string]any) (operation.Spec, error) {
	url, ok := args["url"].(string)
	if !ok || url == "" {
		return operation.Spec{}, fmt.Errorf("url is required")
	}
	mode := "single"
	if m, ok := args["mode"].(string); ok && m != "" {
		switch m {
		case "single", "path_list", "origin_variants":
			mode = m
		default:
			return operation.Spec{}, fmt.Errorf("mode %q is not one of single, path_list, origin_variants", m)
		}
	}

	out := map[string]any{"url": url, "mode": mode}
	if paths, ok := args["paths"].([]any); ok {
		out["paths"] = paths
	}
	if origins, ok := args["origins"].([]any); ok {
		out["origins"] = origins
	}
	if headers, ok := args["headers"].(map[string]any); ok && len(headers) > 0 {
		out["headers"] = headers
	}
	if body, ok := args["body"].(string); ok && body != "" {
		out["body"] = body
	}
	out["timeout"] = probeTimeoutArg(args["timeout"])

	return operation.Spec{Type: "probe", Args: out}, nil
}

// probeTimeoutArg normalises the JSON-decoded timeout to seconds.
func probeTimeoutArg(v any) int {
	switch t := v.(type) {
	case int:
		if t > 0 && t <= 120 {
			return t
		}
	case float64:
		if t > 0 && t <= 120 {
			return int(t)
		}
	}
	return 15
}

func (p *ProbeTool) TranslateResult(op operation.Operation) string { return op.Output }

func (p *ProbeTool) Run(spec operation.Spec) (string, error) {
	return p.RunContext(context.Background(), spec)
}

func (p *ProbeTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	return recon.RunSpec(ctx, spec.Args)
}
