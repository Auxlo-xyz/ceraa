package tool

import (
	"context"
	"fmt"
	"strings"

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

// browserActionDef is one tool: the action it performs, the name it is called,
// and the arguments it accepts.
//
// tool and action are separate because they are not always the same. The
// combined browser opens a page from either a web address or a local file, and
// "either of two" cannot be written down as a required list, so the model was
// handed a schema where nothing was mandatory and an empty call was well formed.
// Two tools, each with one required argument, is the same lesson as the split
// itself: a choice belongs in the tool's identity, not inside its schema.
type browserActionDef struct {
	action string
	// tool is the name suffix. Empty means the action's own name.
	tool string
	// rules overrides the action's arguments for this tool when it takes a
	// different set. Empty means use browserArgSpec for the action.
	rules       *browserArgRules
	description string
}

// toolSuffix is the name this tool is called by.
func (d browserActionDef) toolSuffix() string {
	if d.tool != "" {
		return d.tool
	}
	return d.action
}

// argRules is the argument set this tool accepts.
func (d browserActionDef) argRules() browserArgRules {
	if d.rules != nil {
		return *d.rules
	}
	return browserArgSpec[d.action]
}

// The descriptions say what each tool is and nothing about when to reach for
// it. That is the project's split, enforced by TestDescriptionsCarryNoUsageGuidance:
// usage guidance lives in the prompt's toolrules section, which is where the
// model reads it as a rule rather than as a footnote on one of nine schemas.
var browserActionDefs = []browserActionDef{
	{
		action: "open",
		rules:  &browserArgRules{required: []string{"url"}, optional: []string{"wait_ms"}},
		description: "Load a web page into the browser from its address. " +
			"Replaces whatever page was open.",
	},
	{
		action: "open",
		tool:   "open_file",
		rules:  &browserArgRules{required: []string{"path"}, optional: []string{"wait_ms"}},
		description: "Load a local file into the browser, which is what renders HTML you have just written. " +
			"Replaces whatever page was open.",
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
	base *BrowserTool
	def  browserActionDef
}

func (t *BrowserActionTool) Name() string { return "browser_" + t.def.toolSuffix() }

func (t *BrowserActionTool) Description() string { return t.def.description }

func (t *BrowserActionTool) Schema() map[string]any {
	spec := t.def.argRules()
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

// Translate pins the action and defers the per-argument work to the combined
// tool, so the URL check and the shape checks are the same code they always were.
//
// The argument set is checked here, against this tool's own rules, because the
// combined tool's rules are wider than this tool's. browser_open takes a url and
// browser_open_file takes a path; deferring that would let either tool accept the
// other's argument, which is the same class of mistake as the bundling this split
// removed.
func (t *BrowserActionTool) Translate(args map[string]any) (operation.Spec, error) {
	rules := t.def.argRules()
	allowed := map[string]bool{}
	for _, key := range append(append([]string{}, rules.required...), rules.optional...) {
		allowed[key] = true
	}
	merged := make(map[string]any, len(args)+1)
	merged["action"] = t.def.action
	for key, value := range args {
		if key == "action" {
			// A model that names an action anyway has guessed, and the guess is
			// more useful as a rejection than as something quietly overridden.
			return operation.Spec{}, fmt.Errorf(
				"%s is a single-action tool and takes no %q argument; drop it", t.Name(), "action")
		}
		if !allowed[key] {
			return operation.Spec{}, fmt.Errorf("%s does not accept %q; %s.%s",
				t.Name(), key, describeBrowserArgs(rules), t.def.siblingHint(key))
		}
		merged[key] = value
	}
	var missing []string
	for _, key := range rules.required {
		if _, present := merged[key]; !present {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return operation.Spec{}, fmt.Errorf("%s requires %s", t.Name(), strings.Join(missing, " and "))
	}
	return t.base.Translate(merged)
}

// siblingHint names the tool that does take an argument this one refuses, so a
// crossed argument is one step from working rather than a dead end.
//
// Siblings of the same action are preferred. `path` belongs to browser_open_file
// and also to browser_screenshot, so counting every owner finds two and gives up,
// but in the context of opening a page there is only one sensible answer. Only
// when the action does not narrow it does it fall back to naming the tool when
// there is exactly one home, because an ambiguous hint is worse than none.
func (d browserActionDef) siblingHint(key string) string {
	var sameAction, elsewhere []string
	for _, other := range browserActionDefs {
		if other.action == d.action && other.tool == d.tool {
			continue
		}
		rules := other.argRules()
		for _, accepted := range append(append([]string{}, rules.required...), rules.optional...) {
			if accepted != key {
				continue
			}
			name := "browser_" + other.toolSuffix()
			if other.action == d.action {
				sameAction = append(sameAction, name)
			} else {
				elsewhere = append(elsewhere, name)
			}
			break
		}
	}
	if len(sameAction) == 1 {
		return fmt.Sprintf(" %q belongs to %s", key, sameAction[0])
	}
	if len(sameAction) == 0 && len(elsewhere) == 1 {
		return fmt.Sprintf(" %q belongs to %s", key, elsewhere[0])
	}
	return ""
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
func (t *BrowserActionTool) Sequential() bool        { return true }
func (t *BrowserActionTool) SequentialQueue() string { return "browser" }

// BrowserActionTools returns the per-action tools over one shared browser.
func BrowserActionTools(base *BrowserTool) []Tool {
	tools := make([]Tool, 0, len(browserActionDefs))
	for _, def := range browserActionDefs {
		tools = append(tools, &BrowserActionTool{base: base, def: def})
	}
	return tools
}
