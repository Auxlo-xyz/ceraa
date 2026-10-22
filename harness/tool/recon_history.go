package tool

// recon_history.go reads the receipt trail back and replays it.
//
// The receipts are written by every probe. This makes them queryable: what
// went to this host, what came back, and what happens when the same request
// is sent again with one thing changed. That last one is the primitive
// behind every authentication and access control test, and it only exists
// because the request was recorded.

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/history"
	"ceraa/harness/operation"
	"ceraa/internal/paths"
)

// HistoryTool queries and replays the receipt trail.
type HistoryTool struct{}

func (h *HistoryTool) Name() string { return "history" }

func (h *HistoryTool) Description() string {
	return "Read the receipt trail, and replay a recorded request with one thing changed: the method, the url, the body, a header or a query parameter."
}

func (h *HistoryTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode":           map[string]any{"type": "string", "enum": []string{"list", "replay", "hosts"}, "description": "list, replay or hosts"},
			"id":             map[string]any{"type": "string", "description": "The receipt id to replay"},
			"host":           map[string]any{"type": "string", "description": "Only this host"},
			"path_contains":  map[string]any{"type": "string", "description": "Only urls containing this"},
			"method":         map[string]any{"type": "string", "description": "Only this method"},
			"status":         map[string]any{"type": "integer", "description": "Only this status"},
			"limit":          map[string]any{"type": "integer", "description": "Entries to return, default 50"},
			"new_method":     map[string]any{"type": "string", "description": "The method to send"},
			"new_url":        map[string]any{"type": "string", "description": "The url to send"},
			"body":           map[string]any{"type": "string", "description": "The body to send"},
			"headers":        map[string]any{"type": "object", "additionalProperties": true, "description": "Headers to add"},
			"remove_headers": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Headers to drop"},
			"query":          map[string]any{"type": "object", "additionalProperties": true, "description": "Query parameters to set"},
		},
		"required": []string{},
	}
}

// Translate validates the shape and re-emits the canonical argument map.
func (h *HistoryTool) Translate(args map[string]any) (operation.Spec, error) {
	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = "list"
	}
	switch mode {
	case "list", "replay", "hosts":
	default:
		return operation.Spec{}, fmt.Errorf("mode %q is not one of list, replay, hosts", mode)
	}
	out := map[string]any{"mode": mode}
	for _, key := range []string{"id", "host", "path_contains", "method", "new_method", "new_url", "body"} {
		if value, ok := args[key].(string); ok && value != "" {
			out[key] = value
		}
	}
	if value, ok := args["status"].(float64); ok {
		out["status"] = int(value)
	}
	if value, ok := args["limit"].(float64); ok && value > 0 {
		out["limit"] = int(value)
	}
	for _, key := range []string{"headers", "query"} {
		if value, ok := args[key].(map[string]any); ok && len(value) > 0 {
			out[key] = value
		}
	}
	if list, ok := args["remove_headers"].([]any); ok && len(list) > 0 {
		out["remove_headers"] = list
	}
	if mode == "replay" {
		if _, ok := out["id"]; !ok {
			return operation.Spec{}, fmt.Errorf("mode replay needs the id of a recorded request")
		}
	}
	return operation.Spec{Type: "history", Args: out}, nil
}

func (h *HistoryTool) TranslateResult(op operation.Operation) string { return op.Output }

func (h *HistoryTool) Run(spec operation.Spec) (string, error) {
	return h.RunContext(context.Background(), spec)
}

func (h *HistoryTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	mode, _ := spec.Args["mode"].(string)
	switch mode {
	case "hosts":
		hosts, err := history.Hosts(receiptsDir())
		if err != nil {
			return "", err
		}
		return "hosts: " + strings.Join(hosts, ", "), nil
	case "replay":
		return h.replay(ctx, spec)
	default:
		return h.list(spec)
	}
}

// list prints the recorded requests.
func (h *HistoryTool) list(spec operation.Spec) (string, error) {
	filter := history.Filter{
		Host:         coverageStringArg(spec.Args["host"]),
		PathContains: coverageStringArg(spec.Args["path_contains"]),
		Method:       coverageStringArg(spec.Args["method"]),
		Limit:        50,
	}
	if status, ok := spec.Args["status"].(int); ok {
		filter.Status = status
	}
	if limit, ok := spec.Args["limit"].(int); ok && limit > 0 {
		if limit > 500 {
			limit = 500
		}
		filter.Limit = limit
	}
	entries, err := history.Query(receiptsDir(), filter)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "history: %d entries\n", len(entries))
	for _, entry := range entries {
		fmt.Fprintf(&b, "%s %s %s status %d sha256:%s receipt %s",
			entry.At.Format("2006-01-02T15:04:05Z"),
			entry.Method,
			entry.URL,
			entry.Status,
			shortHash(entry.SHA256),
			entry.ID)
		if entry.RedirectedTo != "" {
			fmt.Fprintf(&b, " redirect %s", entry.RedirectedTo)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// replay sends a recorded request again with the change applied.
func (h *HistoryTool) replay(ctx context.Context, spec operation.Spec) (string, error) {
	id := coverageStringArg(spec.Args["id"])
	entries, err := history.Query(receiptsDir(), history.Filter{})
	if err != nil {
		return "", err
	}
	var entry history.Entry
	found := false
	for _, candidate := range entries {
		if candidate.ID == id {
			entry = candidate
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("no recorded request with id %q", id)
	}
	change := history.Change{
		Method: coverageStringArg(spec.Args["new_method"]),
		URL:    coverageStringArg(spec.Args["new_url"]),
		Body:   coverageStringArg(spec.Args["body"]),
	}
	if headers, ok := spec.Args["headers"].(map[string]any); ok {
		change.Headers = map[string]string{}
		for name, value := range headers {
			switch typed := value.(type) {
			case string:
				change.Headers[name] = typed
			case bool, int, float64:
				change.Headers[name] = fmt.Sprint(typed)
			}
		}
	}
	if list, ok := spec.Args["remove_headers"].([]any); ok {
		for _, item := range list {
			if name, ok := item.(string); ok && name != "" {
				change.RemoveHeaders = append(change.RemoveHeaders, name)
			}
		}
	}
	if query, ok := spec.Args["query"].(map[string]any); ok {
		change.Query = map[string]string{}
		for name, value := range query {
			switch typed := value.(type) {
			case string:
				change.Query[name] = typed
			case bool, int, float64:
				change.Query[name] = fmt.Sprint(typed)
			}
		}
	}
	result, err := history.Replay(ctx, entry, change)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "replay %s %s\n", result.Method, result.URL)
	fmt.Fprintf(&b, "status %d in %dms, body %dB, sha256:%s\n", result.Status, result.ElapsedMS, result.BodyBytes, shortHash(result.SHA256))
	if result.RedirectedTo != "" {
		fmt.Fprintf(&b, "redirect %s (not followed)\n", result.RedirectedTo)
	}
	if result.ReceiptID != "" {
		fmt.Fprintf(&b, "receipt %s\n", result.ReceiptID)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// shortHash trims a hash for display.
func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// receiptsDir is where the probe writes its receipts.
func receiptsDir() string {
	return paths.DataDir() + "/recon/receipts"
}
