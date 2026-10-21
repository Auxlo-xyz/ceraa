package tool

import (
	"encoding/json"
	"strings"
	"testing"
)

// Static wiring tests only: the probe tool's Translate must refuse a call
// without a url, and must emit the canonical spec shape the operation manager
// executes. Behaviour lives in the recon package, which has its own suite.

func TestProbeToolRefusesToTranslateWithoutURL(t *testing.T) {
	var p ProbeTool
	if _, err := p.Translate(map[string]any{}); err == nil || !strings.Contains(err.Error(), "url is required") {
		t.Fatalf("Translate without url: error = %v", err)
	}
	if _, err := p.Translate(map[string]any{"url": ""}); err == nil {
		t.Fatal("Translate with an empty url accepted")
	}
}

func TestProbeToolTranslateWiresTypeModeAndDefaults(t *testing.T) {
	var p ProbeTool
	spec, err := p.Translate(map[string]any{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}
	if spec.Type != "probe" {
		t.Errorf("spec.Type = %q, want probe", spec.Type)
	}
	if mode, _ := spec.Args["mode"].(string); mode != "single" {
		t.Errorf("mode = %q, want the single default", mode)
	}
	if timeout, _ := spec.Args["timeout"].(int); timeout != 15 {
		t.Errorf("timeout = %d, want the 15 default", timeout)
	}

	spec, err = p.Translate(map[string]any{
		"url":     "https://example.com",
		"mode":    "path_list",
		"paths":   []any{"/a", "/b"},
		"origins": []any{"https://app.example.com"},
		"headers": map[string]any{"X-Test": "1"},
		"method":  "POST",
		"body":    "payload",
		"timeout": float64(30),
	})
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}
	if mode, _ := spec.Args["mode"].(string); mode != "path_list" {
		t.Errorf("mode = %q", mode)
	}
	if len(spec.Args["paths"].([]any)) != 2 {
		t.Errorf("paths = %v", spec.Args["paths"])
	}
	if hs, ok := spec.Args["headers"].(map[string]any); !ok || hs["X-Test"] != "1" {
		t.Errorf("headers = %v", spec.Args["headers"])
	}
	if spec.Args["body"] != "payload" || spec.Args["timeout"] != 30 {
		t.Errorf("body/timeout = %v/%v", spec.Args["body"], spec.Args["timeout"])
	}
}

func TestProbeToolTranslateRejectsUnknownMode(t *testing.T) {
	var p ProbeTool
	if _, err := p.Translate(map[string]any{"url": "https://example.com", "mode": "burst"}); err == nil {
		t.Fatal("unknown mode accepted in Translate")
	}
}

// TestProbeToolSchemaAllowsHeaderAndJSONBody pins the fix for two live
// failures: the strict validator rejected every custom header (headers had
// no additionalProperties) and rejected an object body (body was typed
// string), which killed the POST webhook probes.
func TestProbeToolSchemaAllowsHeaderAndJSONBody(t *testing.T) {
	var p ProbeTool
	err := ValidateArguments(p.Schema(), map[string]any{
		"url":     "https://example.com/api/hook",
		"method":  "POST",
		"headers": map[string]any{"Content-Type": "application/json", "X-Count": 1},
		"body":    map[string]any{"event": "charge.success"},
	})
	if err != nil {
		t.Fatalf("schema rejected a legal POST probe: %v", err)
	}
}

func TestProbeToolTranslateNormalizesMethodBodyAndHeaders(t *testing.T) {
	var p ProbeTool
	spec, err := p.Translate(map[string]any{
		"url":     "https://example.com/api/hook",
		"method":  "post",
		"headers": map[string]any{"X-Count": 1},
		"body":    map[string]any{"event": "ping"},
	})
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}
	if method, _ := spec.Args["method"].(string); method != "POST" {
		t.Errorf("method = %v, want POST", spec.Args["method"])
	}
	body, _ := spec.Args["body"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("body is not JSON: %q", body)
	}
	if payload["event"] != "ping" {
		t.Errorf("payload = %v", payload)
	}
	headers, _ := spec.Args["headers"].(map[string]any)
	if headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type = %v, want the JSON default", headers["Content-Type"])
	}
	if headers["X-Count"] != "1" {
		t.Errorf("X-Count = %v, want the coerced string", headers["X-Count"])
	}
}

func TestProbeToolTranslateMethodGuards(t *testing.T) {
	var p ProbeTool
	spec, err := p.Translate(map[string]any{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}
	if method, _ := spec.Args["method"].(string); method != "GET" {
		t.Errorf("default method = %v, want GET", spec.Args["method"])
	}
	if _, err := p.Translate(map[string]any{"url": "https://example.com", "body": "x"}); err == nil || !strings.Contains(err.Error(), "requires POST") {
		t.Errorf("GET with a body: error = %v", err)
	}
	if _, err := p.Translate(map[string]any{"url": "https://example.com", "method": "trace"}); err == nil {
		t.Error("TRACE accepted")
	}
	if _, err := p.Translate(map[string]any{"url": "https://example.com", "mode": "origin_variants", "method": "POST"}); err == nil || !strings.Contains(err.Error(), "GET-only") {
		t.Errorf("POST origin_variants: error = %v", err)
	}
}
