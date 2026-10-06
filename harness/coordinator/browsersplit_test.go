package coordinator

import (
	"encoding/json"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/tool"
)

// A session recorded before the split still contains `browser` calls. Left in
// the history they teach the model the shape that caused the problem, so they
// are rewritten to the per-action names on the way out.
func TestRecordedBrowserCallsBecomeTheActionTools(t *testing.T) {
	items := []llm.Item{
		{Kind: "tool_call", CallID: "c1", Name: "browser",
			Args: `{"action":"open","url":"https://example.com","wait_ms":500}`},
		{Kind: "text", Role: llm.RoleTool, CallID: "c1", Text: "opened"},
		{Kind: "tool_call", CallID: "c2", Name: "browser",
			Args: `{"action":"type","selector":"#q","text":"hello","submit":true}`},
		{Kind: "text", Role: llm.RoleTool, CallID: "c2", Text: "typed"},
	}

	got := normalizeDispatcherMessages(items)

	if got[0].Name != "browser_open" {
		t.Errorf("first call name = %q, want browser_open", got[0].Name)
	}
	var openArgs map[string]any
	if err := json.Unmarshal([]byte(got[0].Args), &openArgs); err != nil {
		t.Fatalf("open args: %v", err)
	}
	if _, stillThere := openArgs["action"]; stillThere {
		t.Error("the action survived into the arguments, so the tool would be told what it already is")
	}
	if openArgs["url"] != "https://example.com" || openArgs["wait_ms"] != float64(500) {
		t.Errorf("open args = %v, want the url and wait kept", openArgs)
	}
	if got[2].Name != "browser_type" {
		t.Errorf("second call name = %q, want browser_type", got[2].Name)
	}
	// The tool result takes the name of the call it answers, or the pair
	// disagrees about which tool ran.
	if got[1].Name != "browser_open" {
		t.Errorf("result for c1 = %q, want browser_open", got[1].Name)
	}
	if got[3].Name != "browser_type" {
		t.Errorf("result for c2 = %q, want browser_type", got[3].Name)
	}
}

// A batch carries its calls in one item, so both shapes have to be handled.
func TestRecordedBrowserCallsInABatchAreSplitToo(t *testing.T) {
	items := []llm.Item{{
		Kind: "tool_calls",
		ToolCalls: []llm.ToolCall{
			{ID: "b1", Name: "browser", Args: `{"action":"click","selector":"#go"}`},
			{ID: "b2", Name: "browser", Args: `{"action":"screenshot","full_page":true}`},
		},
	}}
	got := normalizeDispatcherMessages(items)
	if got[0].ToolCalls[0].Name != "browser_click" || got[0].ToolCalls[1].Name != "browser_screenshot" {
		t.Errorf("batch names = %q, %q", got[0].ToolCalls[0].Name, got[0].ToolCalls[1].Name)
	}
}

// The rewrite must be idempotent, because the runner normalises on every
// request and the history is not rewritten in place. Running twice must not turn
// browser_open into browser_open_open.
func TestTheBrowserSplitIsIdempotent(t *testing.T) {
	items := []llm.Item{
		{Kind: "tool_call", CallID: "c1", Name: "browser",
			Args: `{"action":"eval","script":"1"}`},
	}
	once := normalizeDispatcherMessages(items)
	twice := normalizeDispatcherMessages(once)
	if once[0].Name != "browser_eval" || twice[0].Name != "browser_eval" {
		t.Errorf("names after one pass = %q, after two = %q", once[0].Name, twice[0].Name)
	}
	if twice[0].Args != once[0].Args {
		t.Errorf("arguments changed on the second pass: %q then %q", once[0].Args, twice[0].Args)
	}
}

// Anything that cannot be read is left exactly as recorded. Half-rewriting a
// call would be worse than not rewriting it.
func TestAnUnreadableBrowserCallIsLeftAlone(t *testing.T) {
	for _, raw := range []string{``, `not json`, `{}`, `{"action":""}`, `{"action":7}`, `[]`} {
		item := llm.Item{Kind: "tool_call", CallID: "c1", Name: "browser", Args: raw}
		got := normalizeDispatcherMessages([]llm.Item{item})
		if got[0].Name != "browser" || got[0].Args != raw {
			t.Errorf("args %q became name %q args %q, want it untouched", raw, got[0].Name, got[0].Args)
		}
	}
}

// Only the browser is rewritten. A tool that merely starts with "browser" is a
// different tool, and a call to it must survive.
func TestOtherBrowserShapedToolNamesAreNotRewritten(t *testing.T) {
	item := llm.Item{Kind: "tool_call", CallID: "c1", Name: "browser_thing",
		Args: `{"action":"open","url":"https://example.com"}`}
	got := normalizeDispatcherMessages([]llm.Item{item})
	if got[0].Name != "browser_thing" {
		t.Errorf("name = %q, want it untouched", got[0].Name)
	}
}

// After the rewrite, every browser call in a history names a tool that exists.
// This is the property that matters: a name the catalogue does not offer would
// be a call the model cannot repeat.
func TestEveryRewrittenBrowserNameIsAToolTheModelIsOffered(t *testing.T) {
	offered := map[string]bool{}
	for _, tool := range tool.DefaultRegistry().Offered() {
		offered[tool.Name()] = true
	}
	items := []llm.Item{
		{Kind: "tool_call", CallID: "c1", Name: "browser",
			Args: `{"action":"open","url":"https://example.com"}`},
		{Kind: "tool_call", CallID: "c2", Name: "browser", Args: `{"action":"screenshot"}`},
		{Kind: "tool_call", CallID: "c3", Name: "browser", Args: `{"action":"text"}`},
		{Kind: "tool_call", CallID: "c4", Name: "browser", Args: `{"action":"html"}`},
		{Kind: "tool_call", CallID: "c5", Name: "browser", Args: `{"action":"click","selector":"#"}`},
		{Kind: "tool_call", CallID: "c6", Name: "browser", Args: `{"action":"type","selector":"#","text":"x"}`},
		{Kind: "tool_call", CallID: "c7", Name: "browser", Args: `{"action":"eval","script":"1"}`},
		{Kind: "tool_call", CallID: "c8", Name: "browser", Args: `{"action":"status"}`},
		{Kind: "tool_call", CallID: "c9", Name: "browser", Args: `{"action":"close"}`},
	}
	for _, item := range normalizeDispatcherMessages(items) {
		if !offered[item.Name] {
			t.Errorf("rewrote to %q, which the model is not offered", item.Name)
		}
	}
	if strings.TrimSpace(items[0].Args) == "" {
		t.Fatal("sanity")
	}
}
