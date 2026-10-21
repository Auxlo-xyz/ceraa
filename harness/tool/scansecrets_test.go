package tool

import (
	"testing"

	"ceraa/harness/operation"
)

// TestScanSecretsTranslateRequiresURL: without a url there is no operation,
// and the error says so rather than failing later at the fetch.
func TestScanSecretsTranslateRequiresURL(t *testing.T) {
	tl := &ScanSecretsTool{}
	for _, args := range []map[string]any{
		{},
		{"url": ""},
		{"url": 42},
	} {
		if spec, err := tl.Translate(args); err == nil {
			t.Errorf("Translate(%v) accepted, spec = %v", args, spec)
		}
	}
}

// TestScanSecretsTranslateBuildsScanSpec: a url alone yields the default
// asset count; a larger ask is clamped to the hard cap.
func TestScanSecretsTranslateBuildsScanSpec(t *testing.T) {
	tl := &ScanSecretsTool{}

	spec, err := tl.Translate(map[string]any{"url": "https://example.com/"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if spec.Type != "scan_secrets" {
		t.Errorf("spec.Type = %q, want scan_secrets", spec.Type)
	}
	if spec.Args["url"] != "https://example.com/" {
		t.Errorf("spec.Args[url] = %v", spec.Args["url"])
	}
	if assets, _ := spec.Args["assets"].(int); assets != defaultAssetLimit {
		t.Errorf("default assets = %d, want %d", assets, defaultAssetLimit)
	}

	spec, err = tl.Translate(map[string]any{"url": "https://example.com/", "assets": float64(99)})
	if err != nil {
		t.Fatalf("Translate with assets: %v", err)
	}
	if assets, _ := spec.Args["assets"].(int); assets != hardAssetLimit {
		t.Errorf("clamped assets = %d, want %d", assets, hardAssetLimit)
	}
}

// TestScanSecretsName pins the registry name so a rename here is caught where
// the schema and the operation type have to agree.
func TestScanSecretsName(t *testing.T) {
	tl := &ScanSecretsTool{}
	if tl.Name() != "scan_secrets" {
		t.Errorf("Name = %q, want scan_secrets", tl.Name())
	}
}

// TestScanSecretsTranslateResultPassesOutput: result translation is a pass
// through of the recorded output, like webfetch's.
func TestScanSecretsTranslateResultPassesOutput(t *testing.T) {
	tl := &ScanSecretsTool{}
	op := operation.Operation{Output: "findings: 1 over assets\nx y z: a"}
	if got := tl.TranslateResult(op); got != op.Output {
		t.Errorf("TranslateResult = %q, want passthrough", got)
	}
}
