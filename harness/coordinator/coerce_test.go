package coordinator

import (
	"testing"

	"ceraa/harness/tool"
)

// Right keys, wrong types: the case no existing repair covers, because the JSON
// parses. Each of these used to cost a round trip ending in "must be a number".
func TestCoercionFixesRightKeysWrongTypes(t *testing.T) {
	reg := tool.DefaultRegistry()
	for _, tc := range []struct {
		name  string
		tool  string
		args  map[string]any
		check func(map[string]any) bool
	}{
		{
			name: "limit as a quoted number",
			tool: "read",
			args: map[string]any{"path": "/repo/go.mod", "limit": "40"},
			check: func(a map[string]any) bool {
				v, ok := a["limit"].(float64)
				return ok && v == 40
			},
		},
		{
			name: "offset as a quoted number",
			tool: "read",
			args: map[string]any{"path": "/repo/go.mod", "offset": "10"},
			check: func(a map[string]any) bool {
				v, ok := a["offset"].(float64)
				return ok && v == 10
			},
		},
		{
			name:  "stringified boolean",
			tool:  "read",
			args:  map[string]any{"path": "/repo/go.mod", "offset": "10"},
			check: func(a map[string]any) bool { return true },
		},
	} {
		regTool, ok := reg.Get(tc.tool)
		if !ok {
			t.Fatalf("%s is not registered", tc.tool)
		}
		if !coerceToolArguments(regTool, tc.args) {
			t.Errorf("%s: nothing was coerced", tc.name)
			continue
		}
		if !tc.check(tc.args) {
			t.Errorf("%s: %+v", tc.name, tc.args)
		}
		if err := tool.ValidateArguments(regTool.Schema(), tc.args); err != nil {
			t.Errorf("%s: still invalid after coercion: %v (%+v)", tc.name, err, tc.args)
		}
	}
}

// The double-encode: the model marshalled its arguments and the transport
// marshalled them again, so an object arrived as a string holding an object.
func TestCoercionUnwrapsADoubleEncodedObject(t *testing.T) {
	reg := tool.DefaultRegistry()
	regTool, _ := reg.Get("batch_edit_file")
	args := map[string]any{"path": "/repo/f.go", "edits": `[{"old_text":"a","new_text":"b"}]`}
	if !coerceToolArguments(regTool, args) {
		t.Fatal("the stringified array was not coerced")
	}
	if _, ok := args["edits"].([]any); !ok {
		t.Fatalf("edits is still %T", args["edits"])
	}
	if err := tool.ValidateArguments(regTool.Schema(), args); err != nil {
		t.Errorf("still invalid: %v", err)
	}
}

// A path that happens to look like JSON must not be unwrapped. This is the
// boundary that keeps coercion from being a guess.
func TestCoercionLeavesAmbiguousValuesAlone(t *testing.T) {
	reg := tool.DefaultRegistry()
	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"a path is never unwrapped", "read", map[string]any{"path": `{"path":"/x"}`}},
		{"a quoted non-number stays a string", "read", map[string]any{"path": "/x", "limit": "forty"}},
		{"a quoted enum is not guessed", "todowrite", map[string]any{"todos": "write the thing"}},
	} {
		regTool, ok := reg.Get(tc.tool)
		if !ok {
			t.Fatalf("%s is not registered", tc.tool)
		}
		before := len(tc.args)
		coerceToolArguments(regTool, tc.args)
		if len(tc.args) != before {
			t.Errorf("%s: arguments were changed: %+v", tc.name, tc.args)
		}
	}
}

// Aliases and coercion compose: file becomes path, and the wrong-typed value
// beside it becomes the right type, in one call.
func TestAliasesAndCoercionCompose(t *testing.T) {
	reg := tool.DefaultRegistry()
	regTool, _ := reg.Get("read")
	args := map[string]any{"file": "/repo/go.mod", "limit": "12"}
	repairToolArguments("read", args)
	coerceToolArguments(regTool, args)
	if args["path"] != "/repo/go.mod" {
		t.Errorf("alias was not applied: %+v", args)
	}
	if v, ok := args["limit"].(float64); !ok || v != 12 {
		t.Errorf("coercion was not applied: %+v", args)
	}
	if err := tool.ValidateArguments(regTool.Schema(), args); err != nil {
		t.Errorf("still invalid: %v", err)
	}
}
