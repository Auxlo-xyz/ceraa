package tool

import (
	"strings"
	"testing"
)

// toolList gives the catalogue assertions a name to search by.
type toolList []Tool

func (l toolList) find(name string) (Tool, bool) { return toolSliceFind(l, name) }

func (l toolList) mustFind(t *testing.T, name string) Tool {
	t.Helper()
	tool, ok := toolSliceFind(l, name)
	if !ok {
		t.Fatalf("%s is not in the catalogue", name)
	}
	return tool
}

// The split is only worth anything if the model cannot express the mistake it
// kept making. Each of these pins a property that does not exist on a tool.

func TestEachBrowserActionIsItsOwnTool(t *testing.T) {
	reg := DefaultRegistry()
	for _, want := range []string{
		"browser_open", "browser_screenshot", "browser_text", "browser_html",
		"browser_click", "browser_type", "browser_eval", "browser_status", "browser_close",
	} {
		if _, ok := toolList(reg.Offered()).find(want); !ok {
			t.Errorf("%s is not offered to the model", want)
		}
	}
}

func TestTheCombinedBrowserIsNotOfferedButStillRuns(t *testing.T) {
	reg := DefaultRegistry()
	// Hidden from the catalogue, or the model is taught both shapes.
	if _, ok := toolList(reg.Offered()).find("browser"); ok {
		t.Error("the combined browser is still offered, so both shapes reach the model")
	}
	// Still registered, or a session recorded before the split cannot replay.
	tool, ok := reg.Get("browser")
	if !ok {
		t.Fatal("the combined browser is not registered, so recorded calls cannot run")
	}
	spec, err := tool.Translate(map[string]any{"action": "open", "url": "https://example.com"})
	if err != nil {
		t.Fatalf("a recorded browser call no longer translates: %v", err)
	}
	if spec.Type != "browser" || spec.Args["action"] != "open" {
		t.Errorf("spec = %+v, want the combined browser spec", spec)
	}
}

// The properties that caused every failure are gone from the tool that failed.
func TestOpenCannotCarryAnArgumentFromAnotherAction(t *testing.T) {
	open := toolList(DefaultRegistry().Offered()).mustFind(t, "browser_open")
	properties := open.Schema()["properties"].(map[string]any)
	for _, absent := range []string{"selector", "submit", "text", "script", "full_page"} {
		if _, present := properties[absent]; present {
			t.Errorf("browser_open still advertises %q, which is how the bundling was expressible", absent)
		}
	}
	for _, present := range []string{"url", "path", "wait_ms"} {
		if _, ok := properties[present]; !ok {
			t.Errorf("browser_open is missing %q", present)
		}
	}
}

func TestEachSplitToolCarriesOnlyItsOwnArguments(t *testing.T) {
	reg := DefaultRegistry()
	cases := map[string][]string{
		"browser_open":       {"url", "path", "wait_ms"},
		"browser_click":      {"selector", "wait_ms"},
		"browser_type":       {"selector", "text", "submit", "wait_ms"},
		"browser_screenshot": {"path", "selector", "full_page", "wait_ms"},
		"browser_eval":       {"script", "wait_ms"},
		"browser_status":     {},
		"browser_close":      {},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			properties := toolList(reg.Offered()).mustFind(t, name).Schema()["properties"].(map[string]any)
			if len(properties) != len(want) {
				var got []string
				for key := range properties {
					got = append(got, key)
				}
				t.Errorf("properties = %v, want exactly %v", got, want)
			}
			for _, key := range want {
				if _, ok := properties[key]; !ok {
					t.Errorf("%s is missing %q", name, key)
				}
			}
		})
	}
}

// The required arguments survive the split, or the model is offered a tool it
// can call with nothing in it.
func TestRequiredArgumentsSurviveTheSplit(t *testing.T) {
	reg := DefaultRegistry()
	for name, want := range map[string][]string{
		"browser_click": {"selector"},
		"browser_type":  {"selector", "text"},
		"browser_eval":  {"script"},
	} {
		schema := toolList(reg.Offered()).mustFind(t, name).Schema()
		required, ok := schema["required"].([]string)
		if !ok {
			t.Errorf("%s declares no required arguments", name)
			continue
		}
		if strings.Join(required, ",") != strings.Join(want, ",") {
			t.Errorf("%s required = %v, want %v", name, required, want)
		}
	}
	// open needs a target but takes either of two, so a required list would be
	// wrong; the rule is enforced on translation instead.
	if _, declared := toolList(reg.Offered()).mustFind(t, "browser_open").Schema()["required"]; declared {
		t.Error("browser_open declares required arguments, but either url or path is enough")
	}
}

// Every tool still runs through the one browser, which is what makes the
// sequence meaningful rather than eight separate browsers.
func TestSplitToolsShareOneBrowser(t *testing.T) {
	base := NewBrowserTool()
	for _, tool := range BrowserActionTools(base) {
		actionTool, ok := tool.(*BrowserActionTool)
		if !ok {
			t.Fatalf("%T is not a BrowserActionTool", tool)
		}
		if actionTool.base != base {
			t.Errorf("%s does not share the browser it was built with", tool.Name())
		}
		if !tool.(SequentialTool).Sequential() {
			t.Errorf("%s is not sequential, so a batch could interleave two page steps", tool.Name())
		}
	}
}

// Translating pins the action, so the arguments still go through the one
// validation path rather than a second copy of it.
func TestSplitToolsTranslateIntoTheCombinedSpec(t *testing.T) {
	reg := DefaultRegistry()
	spec, err := toolList(reg.Offered()).mustFind(t, "browser_click").
		Translate(map[string]any{"selector": "#go"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if spec.Type != "browser" || spec.Args["action"] != "click" || spec.Args["selector"] != "#go" {
		t.Errorf("spec = %+v, want a browser click spec", spec)
	}
	// open still refuses a target it does not have, from the shared validation.
	if _, err := toolList(reg.Offered()).mustFind(t, "browser_open").
		Translate(map[string]any{"wait_ms": 100}); err == nil {
		t.Error("browser_open accepted a call with neither url nor path")
	}
}

// A model that names an action on a single-action tool has guessed. Saying so is
// better than quietly overriding it.
func TestASingleActionToolRejectsAnActionArgument(t *testing.T) {
	_, err := toolList(DefaultRegistry().Offered()).mustFind(t, "browser_open").
		Translate(map[string]any{"action": "click", "url": "https://example.com"})
	if err == nil {
		t.Fatal("expected the call to be rejected")
	}
	if !strings.Contains(err.Error(), "single-action tool") {
		t.Errorf("message = %q, want it to explain the tool takes no action argument", err)
	}
}

// The description has to say what the tool is for and when, since it is the
// only thing telling the model which of nine tools to reach for.
func TestEveryBrowserToolExplainsItself(t *testing.T) {
	for _, tool := range DefaultRegistry().Offered() {
		if !strings.HasPrefix(tool.Name(), "browser_") {
			continue
		}
		description := tool.Description()
		if len(description) < 40 {
			t.Errorf("%s has a description of %d chars: %q", tool.Name(), len(description), description)
		}
		if strings.HasPrefix(description, "Drive a browser") {
			t.Errorf("%s has the fallback description, so it says nothing about itself", tool.Name())
		}
	}
}

// With nine browser tools in an alphabetical catalogue, the sequence has to be
// taught somewhere. Descriptions say what a tool is and the prompt says when to
// use it, so this pins that the prompt does its job: naming all nine, and giving
// the order a page is used in.
func TestThePromptTeachesTheBrowserSequence(t *testing.T) {
	prompt := readPrompt(t)
	offered := toolList(DefaultRegistry().Offered())
	var names []string
	for _, tool := range offered {
		if strings.HasPrefix(tool.Name(), "browser_") {
			names = append(names, tool.Name())
		}
	}
	if len(names) != 9 {
		t.Fatalf("browser tools = %v, want nine", names)
	}
	// TestPromptNamesEveryRegisteredTool already covers naming, so this covers the
	// part it cannot: that the split, and the shared page, are stated.
	if !strings.Contains(prompt, "one tool per action, no combined one") {
		t.Error("the prompt does not say the browser is split and the combined tool is gone")
	}
	if !strings.Contains(prompt, "The page is shared") {
		t.Error("the prompt does not say the page is shared across the calls")
	}
	// The descriptions must not have taken the guidance back.
	for _, name := range names {
		description := strings.ToLower(offered.mustFind(t, name).Description())
		for _, phrase := range []string{"use this", "rather than", "instead of", "prefer "} {
			if strings.Contains(description, phrase) {
				t.Errorf("%s description carries usage guidance (%q)", name, phrase)
			}
		}
	}
}

func TestTheCombinedSchemaStillCoversEveryArgument(t *testing.T) {
	combined := NewBrowserTool().Schema()["properties"].(map[string]any)
	for key := range browserArgDefs {
		if _, present := combined[key]; !present {
			t.Errorf("the combined schema lost %q", key)
		}
	}
	// The two schemas are built from one table, so a property added to one and
	// not the other is the drift this catches.
	for _, tool := range BrowserActionTools(NewBrowserTool()) {
		properties := tool.Schema()["properties"].(map[string]any)
		for key := range properties {
			if _, known := browserArgDefs[key]; !known {
				t.Errorf("%s advertises %q, which has no definition", tool.Name(), key)
			}
		}
	}
}

// find and mustFind keep the catalogue assertions readable without a helper
// package. The registry is sorted by name, so a missing tool is a real absence.
func toolSliceFind(tools []Tool, name string) (Tool, bool) {
	for _, tool := range tools {
		if tool.Name() == name {
			return tool, true
		}
	}
	return nil, false
}
