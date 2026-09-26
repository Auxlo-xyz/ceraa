package tool

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/llm"
	"ceraa/harness/operation"
)

// VisionClient is the part of a provider a media view needs. It is an interface
// so the tool can be exercised without a model, and so a provider that cannot
// see is a one-line answer rather than a failed run.
type VisionClient interface {
	DescribeMedia(ctx context.Context, media []*llm.MediaAttachment, prompt string) (string, error)
	ModelName() string
	MediaHost() string
}

// ViewImageTool shows the model what is in an image file.
//
// It used to be a stub that returned a placeholder, which let an agent claim it
// had checked a page it had never seen. It now asks the model, and it asks once:
// a model that rejects media is recorded, so the same failure is not paid for on
// every later turn.
type ViewImageTool struct {
	Client VisionClient
}

func (v *ViewImageTool) Name() string { return "view_image" }

func (v *ViewImageTool) Description() string {
	return "Look at an image file and return what is actually in it: layout, visible text, " +
		"alignment, and anything that looks broken, overlapping, or cut off. Use this to check " +
		"your own work, such as after taking a screenshot, instead of asking the user whether it " +
		"looks right. Prefer this over send_media when you need to know what is wrong with " +
		"something; use send_media to hand a finished file to the user."
}

func (v *ViewImageTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path of the image to look at. JPEG, PNG, WebP, GIF, BMP or TIFF.",
			},
			"prompt": map[string]any{
				"type":        "string",
				"description": "What to look for. Defaults to a general description of layout, text and defects.",
			},
		},
		"required": []string{"path"},
	}
}

func (v *ViewImageTool) Translate(args map[string]any) (operation.Spec, error) {
	raw, ok := args["path"].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return operation.Spec{}, fmt.Errorf("path is required")
	}
	if len(raw) > 4096 || strings.ContainsRune(raw, 0) {
		return operation.Spec{}, fmt.Errorf("path is not a usable file path")
	}
	// Refused up front when the model is already known not to see, so a turn is
	// not spent rediscovering it.
	if v.Client != nil {
		if supported, reason := llm.ModelSupportsMedia(v.Client.ModelName()); !supported {
			return operation.Spec{}, fmt.Errorf("%s", llm.MediaRefusalMessage(v.Client.ModelName(), reason))
		}
	}
	spec := operation.Spec{Type: "view_image", Args: map[string]any{
		"path":   strings.TrimSpace(raw),
		"prompt": strings.TrimSpace(stringArg(args, "prompt")),
	}}
	if value, ok := args["session_id"]; ok {
		spec.Args["session_id"] = value
	}
	return spec, nil
}

func (v *ViewImageTool) TranslateResult(op operation.Operation) string {
	if op.Output == "" && op.Error != "" {
		return "Error: " + op.Error
	}
	return op.Output
}

func (v *ViewImageTool) Run(spec operation.Spec) (string, error) {
	return v.RunContext(context.Background(), spec)
}

func (v *ViewImageTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	if v.Client == nil {
		return "", fmt.Errorf("view_image is not available: no model is configured")
	}
	model := v.Client.ModelName()
	if supported, reason := llm.ModelSupportsMedia(model); !supported {
		return "", fmt.Errorf("%s", llm.MediaRefusalMessage(model, reason))
	}

	base, _ := spec.Args["workdir"].(string)
	raw, _ := spec.Args["path"].(string)
	path, err := ResolvePathFrom(base, raw)
	if err != nil {
		return "", err
	}
	attachment, err := llm.LoadMediaAttachment(path)
	if err != nil {
		return "", err
	}
	prompt, _ := spec.Args["prompt"].(string)

	description, err := v.Client.DescribeMedia(ctx, []*llm.MediaAttachment{attachment}, prompt)
	if err != nil {
		// Only a refusal is remembered. A timeout or a rate limit must not
		// permanently cost the model its vision.
		if llm.IsMediaRefusal(err.Error()) {
			llm.RecordMediaRefusal(model, llm.RefusalReason(err))
			return "", fmt.Errorf("%s", llm.MediaRefusalMessage(model, llm.RefusalReason(err)))
		}
		return "", err
	}
	// The attachment itself is never echoed into the conversation. Only the
	// description is stored, so the image is sent on exactly one request and the
	// cached prefix stays small.
	return description, nil
}
