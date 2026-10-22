package tool

// recon_verify_test.go covers the wiring between the tool and the policy:
// the arguments become an observation, the policy decides, and the verdict
// lands on the finding record.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"ceraa/harness/findings"
)

func TestVerifyToolTranslateChecksItsArguments(t *testing.T) {
	var v VerifyTool
	if _, err := v.Translate(map[string]any{}); err == nil || !strings.Contains(err.Error(), "url is required") {
		t.Errorf("Translate without a url: error = %v", err)
	}
	spec, err := v.Translate(map[string]any{
		"url":           "https://example.com/x",
		"method":        "POST",
		"status":        float64(200),
		"response_body": "hello",
		"marker":        "canary-123456",
		"controls":      []any{"marker"},
	})
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}
	if spec.Type != "verify" {
		t.Errorf("spec.Type = %q", spec.Type)
	}
	if status, _ := spec.Args["status"].(int); status != 200 {
		t.Errorf("status = %v", spec.Args["status"])
	}
	if marker, _ := spec.Args["marker"].(string); marker != "canary-123456" {
		t.Errorf("marker = %v", spec.Args["marker"])
	}
}

func TestVerifyToolRejectsAnUnknownControl(t *testing.T) {
	var v VerifyTool
	spec, err := v.Translate(map[string]any{"url": "https://example.com", "controls": []any{"nonsense"}})
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}
	_, err = v.RunContext(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "unknown control") {
		t.Errorf("an unknown control: error = %v", err)
	}
}

func TestVerifyToolRunsThePolicyOverAGivenExchange(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	var v VerifyTool
	spec, err := v.Translate(map[string]any{
		"url":           "https://example.com/x",
		"method":        "GET",
		"request_body":  "id=7",
		"status":        float64(200),
		"response_body": "the record for id 7",
		"baseline_body": "the record for id 6",
		"controls":      []any{"body"},
	})
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}
	out, err := v.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunContext failed: %v", err)
	}
	if !strings.Contains(out, "verdict:") {
		t.Errorf("the output carries no verdict:\n%s", out)
	}
	if !strings.Contains(out, "obligation body:") {
		t.Errorf("the output carries no obligation:\n%s", out)
	}
}

func TestVerifyToolAttachesTheVerdictToAFinding(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CERAA_HOME", home)
	store, err := findings.Open(findingsDir())
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Put(findings.Finding{
		ID:       "candidate-1",
		Title:    "A candidate",
		Severity: "medium",
		Status:   findings.StatusConfirmed,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	var v VerifyTool
	spec, err := v.Translate(map[string]any{
		"id":            saved.ID,
		"url":           "https://example.com/x",
		"status":        float64(200),
		"response_body": "the record",
		"baseline_body": "nothing",
		"controls":      []any{"body"},
	})
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}
	if _, err := v.RunContext(context.Background(), spec); err != nil {
		t.Fatalf("RunContext failed: %v", err)
	}
	updated, ok := store.Get(saved.ID)
	if !ok {
		t.Fatal("the finding is gone")
	}
	if updated.Verdict == "" {
		t.Error("the verdict was not attached to the finding")
	}
	if len(updated.Reasons) == 0 {
		t.Error("the reasons were not attached to the finding")
	}
}

func TestVerifyToolNamesAMissingFinding(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	var v VerifyTool
	spec, err := v.Translate(map[string]any{
		"id":            "nothing-here",
		"url":           "https://example.com/x",
		"status":        float64(200),
		"response_body": "body",
		"controls":      []any{"body"},
	})
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}
	out := &bytes.Buffer{}
	_ = out
	if _, err := v.RunContext(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "no finding with id") {
		t.Errorf("a missing finding: error = %v", err)
	}
}
