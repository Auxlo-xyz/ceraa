package tool

import "testing"

func TestValidateArgumentsRequiresDeclaredFieldsAndTypes(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":  map[string]any{"type": "string", "minLength": 1},
			"count": map[string]any{"type": "integer", "minimum": 1},
		},
		"required": []string{"path", "count"},
	}
	if err := ValidateArguments(schema, map[string]any{"path": "x", "count": 2}); err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{
		{"path": "x"},
		{"path": "x", "count": 1.5},
		{"path": "x", "count": 1, "extra": true},
	} {
		if err := ValidateArguments(schema, args); err == nil {
			t.Fatalf("expected validation error for %#v", args)
		}
	}
}

func TestValidateArgumentsChecksNestedValues(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"todos": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":       "object",
					"properties": map[string]any{"content": map[string]any{"type": "string"}},
					"required":   []string{"content"},
				},
			},
		},
		"required": []string{"todos"},
	}
	if err := ValidateArguments(schema, map[string]any{"todos": []any{map[string]any{"content": "run tests"}}}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateArguments(schema, map[string]any{"todos": []any{map[string]any{"content": 1}}}); err == nil {
		t.Fatal("expected nested type error")
	}
}
