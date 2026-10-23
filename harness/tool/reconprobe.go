package tool

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"ceraa/harness/operation"
	"ceraa/harness/recon"
)

// ProbeTool is the model-facing name of the recon probe. It only shapes
// arguments: scope gating, the rate budget and execution live in the recon
// package, and registration happens in the registry, not here.
type ProbeTool struct{}

func (p *ProbeTool) Name() string { return "probe" }

func (p *ProbeTool) Description() string {
	return "Send one HTTP request and get the observation: status, elapsed, header subset, redirect, body hash and a receipt. Modes: single, path_list (30 paths), origin_variants (with a CORS verdict). Methods: GET, POST, HEAD, OPTIONS, PUT, PATCH, DELETE. Put {{oast}} in the url, a header or the body to carry a unique callback host; a blind bug is then checked by finding verify with the callback control."
}

func (p *ProbeTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":     map[string]any{"type": "string", "description": "Absolute http:// or https:// URL to probe"},
			"mode":    map[string]any{"type": "string", "enum": []string{"single", "path_list", "origin_variants"}, "description": "single (default) fires one request; path_list appends each relative path to url as its own request; origin_variants adds Origin-header variants plus the null origin"},
			"method":  map[string]any{"type": "string", "description": "HTTP method, default GET. One of GET, POST, HEAD, OPTIONS, PUT, PATCH, DELETE. The state-changing verbs are available by default"},
			"paths":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 30, "description": "Relative paths for path_list mode, maximum 30"},
			"origins": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 12, "description": "Origin values for origin_variants mode, maximum 12"},
			"headers": map[string]any{"type": "object", "additionalProperties": true, "description": "Request headers as name/value pairs; values may be strings, numbers or booleans"},
			"body":    map[string]any{"additionalProperties": true, "description": "Request body for POST, PUT or PATCH: either a string, or a JSON object/array which is serialized and sent as application/json. Capped at 64k"},
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

	method, err := probeMethodArg(args["method"])
	if err != nil {
		return operation.Spec{}, err
	}
	if mode == "origin_variants" && method != http.MethodGet {
		return operation.Spec{}, fmt.Errorf("mode origin_variants is GET-only")
	}
	body, bodyIsJSON, err := recon.NormalizeBody(args["body"])
	if err != nil {
		return operation.Spec{}, err
	}
	if body != "" && method != http.MethodPost && method != http.MethodPut && method != http.MethodPatch {
		return operation.Spec{}, fmt.Errorf("a body requires POST, PUT or PATCH, not %s", method)
	}
	headers, err := probeHeaderArgs(args["headers"])
	if err != nil {
		return operation.Spec{}, err
	}
	if bodyIsJSON && body != "" && !probeHasHeader(headers, "Content-Type") {
		if headers == nil {
			headers = make(map[string]any, 1)
		}
		headers["Content-Type"] = "application/json"
	}

	out := map[string]any{"url": url, "mode": mode, "method": method}
	if paths, ok := args["paths"].([]any); ok {
		out["paths"] = paths
	}
	if origins, ok := args["origins"].([]any); ok {
		out["origins"] = origins
	}
	if len(headers) > 0 {
		out["headers"] = headers
	}
	if body != "" {
		out["body"] = body
	}
	out["timeout"] = probeTimeoutArg(args["timeout"])

	return operation.Spec{Type: "probe", Args: out}, nil
}

// probeMethodArg normalises the method and refuses anything outside the
// probe's whitelist. Case is irrelevant: models send "post" as often as
// "POST".
func probeMethodArg(v any) (string, error) {
	raw, _ := v.(string)
	method := strings.ToUpper(strings.TrimSpace(raw))
	if method == "" {
		return http.MethodGet, nil
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodHead, http.MethodOptions,
		http.MethodPut, http.MethodPatch, http.MethodDelete:
		return method, nil
	}
	return "", fmt.Errorf("method %q is not one of GET, POST, HEAD, OPTIONS, PUT, PATCH, DELETE", method)
}

// probeHeaderArgs normalises header values to strings. The schema allows
// scalars because a model that writes {"X-Count": 1} means the header, and
// rejecting it only costs a retry.
func probeHeaderArgs(v any) (map[string]any, error) {
	switch headers := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		out := make(map[string]any, len(headers))
		for name, value := range headers {
			switch typed := value.(type) {
			case string:
				out[name] = typed
			case bool, int, int64, float64, float32:
				out[name] = fmt.Sprint(typed)
			default:
				return nil, fmt.Errorf("header %q value must be a string, number or boolean", name)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("headers must be an object of name/value pairs")
	}
}

// probeHasHeader reports whether a header is already set, ignoring case.
func probeHasHeader(headers map[string]any, name string) bool {
	for key := range headers {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
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
