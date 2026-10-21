package tool

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/operation"
	"ceraa/harness/recon"
)

// defaultAssetLimit and hardAssetLimit mirror recon's own bounds; the tool
// clamps the model's ask before it becomes an operation.
const (
	defaultAssetLimit = 12
	hardAssetLimit    = 30
)

// ScanSecretsTool audits a page and its scripts for embedded secrets. It is
// the recon scanner wearing a tool face: the model asks for a URL, the tool
// returns masked findings, and that is the whole contract.
type ScanSecretsTool struct{}

func (s *ScanSecretsTool) Name() string { return "scan_secrets" }

func (s *ScanSecretsTool) Description() string {
	return "Audit a web page and its same-origin JavaScript files and source maps for embedded secrets and leaked keys. Returns masked findings only, never full values. The scan is read-only: nothing is probed for liveness or submitted to any endpoint, so a finding is a receipt that lands in the report and nowhere else - findings never leave here unless the user reports them."
}

func (s *ScanSecretsTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":    map[string]any{"type": "string", "description": "The HTTP or HTTPS URL of the page to audit"},
			"assets": map[string]any{"type": "integer", "description": fmt.Sprintf("Maximum number of linked JS assets to fetch (default %d, maximum %d)", defaultAssetLimit, hardAssetLimit)},
		},
		"required": []string{"url"},
	}
}

func (s *ScanSecretsTool) Translate(args map[string]any) (operation.Spec, error) {
	url, ok := args["url"].(string)
	if !ok || url == "" {
		return operation.Spec{}, fmt.Errorf("url is required")
	}
	assets := defaultAssetLimit
	if a, ok := args["assets"].(float64); ok && a > 0 {
		assets = int(a)
	}
	if assets > hardAssetLimit {
		assets = hardAssetLimit
	}
	return operation.Spec{Type: "scan_secrets", Args: map[string]any{
		"url":    url,
		"assets": assets,
	}}, nil
}

func (s *ScanSecretsTool) TranslateResult(op operation.Operation) string {
	return op.Output
}

func (s *ScanSecretsTool) Run(spec operation.Spec) (string, error) {
	return s.RunContext(context.Background(), spec)
}

func (s *ScanSecretsTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	url, _ := spec.Args["url"].(string)
	assets, _ := spec.Args["assets"].(int)
	findings, err := recon.ScanAssets(ctx, url, assets)
	if err != nil {
		return "", err
	}
	lines := []string{fmt.Sprintf("findings: %d over assets", len(findings))}
	for _, f := range findings {
		lines = append(lines, fmt.Sprintf("%s %s %s: %s", f.Asset, f.Confidence, f.Name, f.Masked))
	}
	return strings.Join(lines, "\n"), nil
}
