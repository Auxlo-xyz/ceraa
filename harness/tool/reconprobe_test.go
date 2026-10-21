package tool

import (
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
