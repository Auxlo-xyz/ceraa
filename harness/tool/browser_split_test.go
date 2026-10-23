package tool

import (
	"os"
	"path/filepath"
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

// browserToolList returns the registry's browser tools whether or not they are
// offered. They are hidden now that the browser is a shell command, but they
// stay registered so a session recorded before the change still replays.
func browserToolList(reg *Registry) toolList {
	var list toolList
	for _, candidate := range reg.All() {
		if candidate.Name() == "browser" || strings.HasPrefix(candidate.Name(), "browser_") {
			list = append(list, candidate)
		}
	}
	return list
}

// The browser is reached through `ceraa browser` now, so the action tools must
// not be in the catalogue. They stay registered: a recorded call resolves by
// name, so removing them would strand that history.
func TestBrowserActionsAreHiddenButStillExecutable(t *testing.T) {
	reg := DefaultRegistry()
	for _, name := range []string{
		"browser_open", "browser_open_file", "browser_click", "browser_type",
		"browser_eval", "browser_screenshot", "browser_status", "browser_close",
	} {
		if _, ok := toolList(reg.Offered()).find(name); ok {
			t.Errorf("%s is still offered; the browser is a shell command now", name)
		}
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s is not registered, so a recorded call cannot replay", name)
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
	open := browserToolList(DefaultRegistry()).mustFind(t, "browser_open")
	properties := open.Schema()["properties"].(map[string]any)
	// path moved to browser_open_file, and the rest were never this tool's.
	for _, absent := range []string{"selector", "submit", "text", "script", "full_page", "path"} {
		if _, present := properties[absent]; present {
			t.Errorf("browser_open still advertises %q, which is how the bundling was expressible", absent)
		}
	}
	for _, present := range []string{"url", "wait_ms"} {
		if _, ok := properties[present]; !ok {
			t.Errorf("browser_open is missing %q", present)
		}
	}
}

func TestEachSplitToolCarriesOnlyItsOwnArguments(t *testing.T) {
	reg := DefaultRegistry()
	cases := map[string][]string{
		"browser_open":       {"url", "wait_ms"},
		"browser_open_file":  {"path", "wait_ms"},
		"browser_click":      {"selector", "wait_ms"},
		"browser_type":       {"selector", "text", "submit", "wait_ms"},
		"browser_screenshot": {"path", "selector", "full_page", "wait_ms"},
		"browser_eval":       {"script", "wait_ms"},
		"browser_status":     {},
		"browser_close":      {},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			properties := browserToolList(reg).mustFind(t, name).Schema()["properties"].(map[string]any)
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
		schema := browserToolList(reg).mustFind(t, name).Schema()
		required, ok := schema["required"].([]string)
		if !ok {
			t.Errorf("%s declares no required arguments", name)
			continue
		}
		if strings.Join(required, ",") != strings.Join(want, ",") {
			t.Errorf("%s required = %v, want %v", name, required, want)
		}
	}
	// open used to be exempt here, on the reasoning that either url or path is
	// enough so a required list would be wrong. That is what left the schema with
	// nothing mandatory and made an empty call well formed. It is split in two
	// now, so both declare their target.
	for name, want := range map[string][]string{
		"browser_open":      {"url"},
		"browser_open_file": {"path"},
	} {
		required, ok := browserToolList(reg).mustFind(t, name).Schema()["required"].([]string)
		if !ok {
			t.Errorf("%s declares no required argument", name)
			continue
		}
		if strings.Join(required, ",") != strings.Join(want, ",") {
			t.Errorf("%s required = %v, want %v", name, required, want)
		}
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
	spec, err := browserToolList(reg).mustFind(t, "browser_click").
		Translate(map[string]any{"selector": "#go"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if spec.Type != "browser" || spec.Args["action"] != "click" || spec.Args["selector"] != "#go" {
		t.Errorf("spec = %+v, want a browser click spec", spec)
	}
	// open still refuses a target it does not have, from the shared validation.
	if _, err := browserToolList(reg).mustFind(t, "browser_open").
		Translate(map[string]any{"wait_ms": 100}); err == nil {
		t.Error("browser_open accepted a call with neither url nor path")
	}
}

// A model that names an action on a single-action tool has guessed. Saying so is
// better than quietly overriding it.
func TestASingleActionToolRejectsAnActionArgument(t *testing.T) {
	_, err := browserToolList(DefaultRegistry()).mustFind(t, "browser_open").
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

// The prompt is where the sequence lives now: the command, its actions, the
// shared page, and the fact that a ref belongs to the snapshot that produced it.
func TestThePromptTeachesTheBrowserCommand(t *testing.T) {
	prompt := readPrompt(t)
	for _, want := range []string{"ceraa browser", "snapshot", "click @N", "type @N", "persists"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not mention %q", want)
		}
	}
	for _, offered := range DefaultRegistry().Offered() {
		if strings.HasPrefix(offered.Name(), "browser_") {
			t.Errorf("%s is offered, so the prompt and the catalogue disagree", offered.Name())
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

// This is the gap that made browser_open callable with nothing in it. The
// combined tool needs either a url or a path, which cannot be written as a
// required list, so the split's first version left open's schema with no
// required argument at all. A call with no arguments was then well formed and
// only failed at runtime, which the model hit by passing neither.
func TestOpeningIsTwoToolsWithARequiredTarget(t *testing.T) {
	offered := browserToolList(DefaultRegistry())
	for name, required := range map[string]string{
		"browser_open":      "url",
		"browser_open_file": "path",
	} {
		schema := offered.mustFind(t, name).Schema()
		got, ok := schema["required"].([]string)
		if !ok {
			t.Errorf("%s declares no required argument, so a call with nothing in it is well formed", name)
			continue
		}
		if len(got) != 1 || got[0] != required {
			t.Errorf("%s required = %v, want [%s]", name, got, required)
		}
		// And the other target must not be accepted, or the two tools are one
		// tool with two spellings.
		properties := schema["properties"].(map[string]any)
		other := "path"
		if required == "path" {
			other = "url"
		}
		if _, present := properties[other]; present {
			t.Errorf("%s still advertises %q, which belongs to the other open tool", name, other)
		}
	}
}

// The empty call the model actually made.
func TestOpeningWithNothingIsRejectedWithWhatIsNeeded(t *testing.T) {
	for _, name := range []string{"browser_open", "browser_open_file"} {
		_, err := browserToolList(DefaultRegistry()).mustFind(t, name).Translate(map[string]any{})
		if err == nil {
			t.Errorf("%s accepted a call with no arguments", name)
			continue
		}
		if !strings.Contains(err.Error(), "requires") {
			t.Errorf("%s said %q, want it to say what is required", name, err)
		}
	}
}

// A crossed argument has exactly one other home, so the refusal names it.
func TestACrossedTargetNamesTheToolThatTakesIt(t *testing.T) {
	offered := browserToolList(DefaultRegistry())
	_, err := offered.mustFind(t, "browser_open").Translate(map[string]any{"path": "/tmp/x.html"})
	if err == nil {
		t.Fatal("browser_open accepted a path")
	}
	if !strings.Contains(err.Error(), `"path" belongs to browser_open_file`) {
		t.Errorf("message = %q, want it to name the tool that takes a path", err)
	}
	_, err = offered.mustFind(t, "browser_open_file").Translate(map[string]any{"url": "https://example.com"})
	if err == nil {
		t.Fatal("browser_open_file accepted a url")
	}
	if !strings.Contains(err.Error(), `"url" belongs to browser_open`) {
		t.Errorf("message = %q, want it to name the tool that takes a url", err)
	}
}

// The two must still land on the same action, or they are not two spellings of
// one thing.
func TestBothOpenToolsReachTheSameAction(t *testing.T) {
	// A real file: the target is resolved during translation, so a path that
	// does not exist fails before the action is ever reached.
	page := filepath.Join(t.TempDir(), "page.html")
	if err := os.WriteFile(page, []byte("<html><body>ok</body></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	offered := browserToolList(DefaultRegistry())
	web, err := offered.mustFind(t, "browser_open").Translate(map[string]any{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if web.Type != "browser" || web.Args["action"] != "open" || web.Args["url"] != "https://example.com" {
		t.Errorf("web spec = %+v, want the browser open action and URL", web)
	}
	spec, err := offered.mustFind(t, "browser_open_file").Translate(map[string]any{"path": page})
	if err != nil {
		t.Fatalf("open_file: %v", err)
	}
	if spec.Type != "browser" || spec.Args["action"] != "open" {
		t.Errorf("spec = %+v, want the browser open action", spec)
	}
	if _, present := spec.Args["url"]; present {
		t.Error("open_file invented a url")
	}
}
