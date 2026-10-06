package tool

import (
	"context"
	"fmt"

	"ceraa/harness/operation"
)

// The browser used to be one tool with an `action` argument, and the model
// repeatedly bundled arguments belonging to different actions into a single
// call: `open` with a `selector`, with a `submit`, with a `script`. Each was
// well formed JSON carrying real properties, just not valid for the action
// named, and each was rejected. The rejection then had to teach the recovery, and
// even with that it took several attempts.
//
// Twenty of the twenty-one tools in the catalogue are a single verb with a
// single set of properties, and the model had learned that shape from all of
// them. `browser` was the one that was not shaped like the rest.
//
// So the action becomes the tool. A call to `browser_open` cannot carry a
// `selector`, because the property is not in its schema, and a schema is a
// stronger instruction than a description: the descriptions already said which
// arguments belonged where and were ignored.
//
// The dispatch is unchanged. Every one of these translates into the same
// `browser` operation spec, so there is still one driver, one action switch and
// one set of validations, all of them the existing ones.

// browserArgDef is a property, described once so the combined schema and the
// per-action schemas cannot drift apart.
type browserArgDef struct {
	kind        string
	description string
}

// browserArgDefs describes every browser argument. The wording is deliberately
// action-neutral, because these are now rendered into nine separate schemas and a
// description that named one action would be wrong in the other eight.
var browserArgDefs = map[string]browserArgDef{
	"url":       {"string", "Web address to open, http or https."},
	"path":      {"string", "A local file to open, or where to write a screenshot."},
	"selector":  {"string", "CSS selector of the element to act on."},
	"text":      {"string", "Text to type."},
	"script":    {"string", "JavaScript source. The page's result is returned as JSON."},
	"full_page": {"boolean", "Capture the whole scrollable page rather than the viewport."},
	"submit":    {"boolean", "Press Enter after typing."},
	"wait_ms":   {"integer", "Milliseconds to wait after the action, for pages that render late."},
}

// browserActionDef is one tool: the action it performs and what it acts on.
type browserActionDef struct {
	action      string
	description string
}

// The descriptions say what each tool is and nothing about when to reach for
// it. That is the project's split, enforced by TestDescriptionsCarryNoUsageGuidance:
// usage guidance lives in the prompt's toolrules section, which is where the
// model reads it as a rule rather than as a footnote on one of nine schemas.
var browserActionDefs = []browserActionDef{
	{
		action:      "open",
		description: "Load a page into the browser, from a web address or a local file. Replaces whatever page was open. Takes a url or a path, not both.",
	},
	{
		action:      "text",
		description: "Read the visible text of the open page, or of one element on it, given a selector.",
	},
	{
		action:      "html",
		description: "Read the HTML source of the open page, or of one element on it, given a selector.",
	},
	{
		action:      "click",
		description: "Click one element on the open page, named by a CSS selector.",
	},
	{
		action:      "type",
		description: "Type text into one element on the open page, named by a CSS selector. With submit set, presses Enter afterwards.",
	},
	{
		action:      "screenshot",
		description: "Capture the open page, or one element on it, as a PNG image. Writes to the data directory unless a path is given, and returns that path.",
	},
	{
		action:      "eval",
		description: "Run JavaScript in the open page and return its result as JSON.",
	},
	{
		action:      "status",
		description: "Report whether a browser is running and which page it has open. Takes no arguments.",
	},
	{
		action:      "close",
		description: "Close the browser and release it. Takes no arguments.",
	},
}

// BrowserActionTool is one browser action presented as its own tool.
//
// It holds no browser of its own. Every instance shares the one BrowserTool, so
// a page opened by browser_open is the page browser_click acts on, which is the
// entire reason the actions are a sequence rather than independent tools.
type BrowserActionTool struct {
	base   *BrowserTool
	action string
}

func (t *BrowserActionTool) Name() string { return "browser_" + t.action }

func (t *BrowserActionTool) Description() string { return browserActionDescription(t.action) }

func (t *BrowserActionTool) Schema() map[string]any {
	spec, ok := browserArgSpec[t.action]
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	properties := map[string]any{}
	for _, key := range append(append([]string{}, spec.required...), spec.optional...) {
		def, known := browserArgDefs[key]
		if !known {
			continue
		}
		properties[key] = map[string]any{"type": def.kind, "description": def.description}
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if len(spec.required) > 0 {
		schema["required"] = spec.required
	}
	return schema
}

// Translate pins the action and defers everything else to the combined tool, so
// the argument spec, the URL check, the "exactly one of url or path" rule and
// the rejection wording are the same code they always were. Nothing about
// validation is restated here, because restating it is how two copies drift.
func (t *BrowserActionTool) Translate(args map[string]any) (operation.Spec, error) {
	merged := make(map[string]any, len(args)+1)
	merged["action"] = t.action
	for key, value := range args {
		if key == "action" {
			// A model that names an action anyway has guessed, and the guess is
			// more useful as a rejection than as something quietly overridden.
			return operation.Spec{}, fmt.Errorf(
				"%s is a single-action tool and takes no \"action\" argument; drop it", t.Name())
		}
		merged[key] = value
	}
	return t.base.Translate(merged)
}

func (t *BrowserActionTool) TranslateResult(op operation.Operation) string {
	return t.base.TranslateResult(op)
}

func (t *BrowserActionTool) Run(spec operation.Spec) (string, error) {
	return t.base.Run(spec)
}

func (t *BrowserActionTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	return t.base.RunContext(ctx, spec)
}

// Sequential for the same reason the combined tool is: one browser, one page,
// and a batch is a pipeline the model expects in order.
func (t *BrowserActionTool) Sequential() bool { return true }

func browserActionDescription(action string) string {
	for _, def := range browserActionDefs {
		if def.action == action {
			return def.description
		}
	}
	return "Drive the browser."
}

// BrowserActionTools returns the per-action tools over one shared browser.
//
// BrowserActionTools returns the per-action tools over one shared browser.
func BrowserActionTools(base *BrowserTool) []Tool {
	tools := make([]Tool, 0, len(browserActionDefs))
	for _, def := range browserActionDefs {
		tools = append(tools, &BrowserActionTool{base: base, action: def.action})
	}
	return tools
}
