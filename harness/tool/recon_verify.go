package tool

// recon_verify.go is the validation face of the recon layer.
//
// The finding record says what was found. This says whether it holds. The
// two are separate on purpose: a model that both finds and validates its
// own finding has no second opinion, and the second opinion has to be a
// policy rather than another judgement call.
//
// The policy itself lives in harness/verify and fetches nothing. This tool
// is the part that goes and gets the exchanges, so the policy stays pure and
// the network stays here, under the scope gate and the receipt trail.

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/findings"
	"ceraa/harness/operation"
	"ceraa/harness/recon"
	"ceraa/harness/verify"
)

// VerifyTool runs the verification policy over a recorded exchange and
// records the verdict.
type VerifyTool struct{}

func (v *VerifyTool) Name() string { return "verify" }

func (v *VerifyTool) Description() string {
	return "Run the verification policy over an exchange: PROVEN, REFUTED, UNPROVEN or INCONCLUSIVE. It refuses a marker the baseline also carries, refuses a payload that was only reflected, and masks any credential."
}

func (v *VerifyTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":              map[string]any{"type": "string", "description": "The finding to attach the verdict to"},
			"url":             map[string]any{"type": "string", "description": "The URL"},
			"method":          map[string]any{"type": "string", "description": "The method, default GET"},
			"request_body":    map[string]any{"type": "string", "description": "The request body"},
			"status":          map[string]any{"type": "integer", "description": "The status, if known"},
			"response_body":   map[string]any{"type": "string", "description": "The response body, if known"},
			"baseline_url":    map[string]any{"type": "string", "description": "The baseline url, fetched when given"},
			"baseline_status": map[string]any{"type": "integer", "description": "The baseline status, if known"},
			"baseline_body":   map[string]any{"type": "string", "description": "The baseline body, if known"},
			"marker":          map[string]any{"type": "string", "description": "The canary that was injected"},
			"controls":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Controls to run: marker, status, body, reflection, secret"},
		},
		"required": []string{"url"},
	}
}

// Translate validates the shape and re-emits the canonical argument map.
func (v *VerifyTool) Translate(args map[string]any) (operation.Spec, error) {
	url, _ := args["url"].(string)
	if strings.TrimSpace(url) == "" {
		return operation.Spec{}, fmt.Errorf("url is required")
	}
	out := map[string]any{"url": strings.TrimSpace(url)}
	for _, key := range []string{"id", "method", "request_body", "response_body", "baseline_url", "baseline_body", "marker"} {
		if value, ok := args[key].(string); ok && value != "" {
			out[key] = value
		}
	}
	for _, key := range []string{"status", "baseline_status"} {
		if value, ok := args[key].(float64); ok {
			out[key] = int(value)
		}
	}
	if list, ok := args["controls"].([]any); ok && len(list) > 0 {
		out["controls"] = list
	}
	return operation.Spec{Type: "verify", Args: out}, nil
}

func (v *VerifyTool) TranslateResult(op operation.Operation) string { return op.Output }

func (v *VerifyTool) Run(spec operation.Spec) (string, error) {
	return v.RunContext(context.Background(), spec)
}

func (v *VerifyTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	url, _ := spec.Args["url"].(string)
	method, _ := spec.Args["method"].(string)
	if method == "" {
		method = "GET"
	}
	requestBody, _ := spec.Args["request_body"].(string)

	observation := verify.Observation{
		URL:         url,
		Method:      method,
		RequestBody: requestBody,
	}
	status, hasStatus := spec.Args["status"].(int)
	responseBody, hasBody := spec.Args["response_body"].(string)
	if !hasStatus || !hasBody {
		result, body, err := v.fetch(ctx, url, method, requestBody)
		if err != nil {
			return "", err
		}
		if !hasStatus {
			observation.Status = result.Status
		} else {
			observation.Status = status
		}
		if !hasBody {
			observation.ResponseBody = string(body)
		} else {
			observation.ResponseBody = responseBody
		}
		observation.ResponseHeaders = result.Headers
	}

	baselineURL, _ := spec.Args["baseline_url"].(string)
	baselineBody, hasBaselineBody := spec.Args["baseline_body"].(string)
	baselineStatus, hasBaselineStatus := spec.Args["baseline_status"].(int)
	if baselineURL != "" || hasBaselineBody {
		baseline := &verify.Observation{URL: baselineURL, Method: method}
		if baselineURL != "" {
			result, body, err := v.fetch(ctx, baselineURL, method, "")
			if err != nil {
				return "", err
			}
			baseline.Status = result.Status
			baseline.ResponseBody = string(body)
			baseline.ResponseHeaders = result.Headers
		}
		if hasBaselineStatus {
			baseline.Status = baselineStatus
		}
		if hasBaselineBody {
			baseline.ResponseBody = baselineBody
		}
		observation.Baseline = baseline
	}

	controls, err := v.controls(spec.Args["controls"])
	if err != nil {
		return "", err
	}
	decision := verify.Decide(observation, controls)

	// The verdict is worth attaching to the record, because a finding whose
	// verdict lives only in a chat message is a finding that gets reported
	// without it.
	if id, _ := spec.Args["id"].(string); id != "" {
		store, err := findings.Open(findingsDir())
		if err != nil {
			return "", err
		}
		finding, ok := store.Get(id)
		if !ok {
			return "", fmt.Errorf("no finding with id %q", id)
		}
		finding.Verdict = string(decision.Verdict)
		finding.Reasons = decision.Reasons
		if _, err := store.Put(finding); err != nil {
			return "", err
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "verdict: %s\n", decision.Verdict)
	for _, obligation := range decision.Obligations {
		fmt.Fprintf(&b, "obligation %s: %s", obligation.Name, obligation.State)
		if obligation.Detail != "" {
			fmt.Fprintf(&b, " - %s", obligation.Detail)
		}
		if obligation.Evidence != "" {
			fmt.Fprintf(&b, " | %s", obligation.Evidence)
		}
		b.WriteString("\n")
	}
	for _, reason := range decision.Reasons {
		fmt.Fprintf(&b, "reason: %s\n", reason)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// fetch performs one exchange for the verification, through the same gate,
// budget and receipt trail every other request goes through.
func (v *VerifyTool) fetch(ctx context.Context, url string, method string, body string) (recon.Result, []byte, error) {
	if err := recon.Gate(url); err != nil {
		return recon.Result{}, nil, err
	}
	if err := recon.Take(1); err != nil {
		return recon.Result{}, nil, err
	}
	return recon.ProbeCapture(ctx, recon.Request{URL: url, Method: method, Body: body}, 256*1024)
}

// controls picks the controls to run. The default set includes the secret
// control, which contradicts on a response with no credential in it, so a
// caller verifying something that is not a credential picks the ones it
// wants.
func (v *VerifyTool) controls(raw any) ([]verify.Control, error) {
	names, _ := raw.([]any)
	if len(names) == 0 {
		return verify.DefaultControls(), nil
	}
	all := map[string]verify.Control{}
	for _, control := range verify.DefaultControls() {
		all[control.Name] = control
	}
	picked := make([]verify.Control, 0, len(names))
	for _, entry := range names {
		name, _ := entry.(string)
		control, ok := all[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			return nil, fmt.Errorf("unknown control %q; the controls are %s", name, strings.Join(controlNames(all), ", "))
		}
		picked = append(picked, control)
	}
	if len(picked) == 0 {
		return nil, fmt.Errorf("no control was selected")
	}
	return picked, nil
}

// controlNames lists the control names for an error message.
func controlNames(all map[string]verify.Control) []string {
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	return names
}
