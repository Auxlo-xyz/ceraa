package tool

import (
	"fmt"

	"ceraa/harness/operation"
)

// TodoWriteTool maps to soraacode tool/todowrite.ts persisted TodoWrite
type TodoWriteTool struct{}

func (t *TodoWriteTool) Name() string { return "todowrite" }
func (t *TodoWriteTool) Description() string {
	return "Set the session plan. Items are stored on the persistent goal, so get_goal reads them back."
}
func (t *TodoWriteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"todos": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content":  map[string]any{"type": "string"},
						"status":   map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed", "cancelled"}},
						"priority": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
					},
					"required": []string{"content", "status", "priority"},
				},
			},
		},
		"required": []string{"todos"},
	}
}
func (t *TodoWriteTool) Translate(args map[string]any) (operation.Spec, error) {
	todos, ok := args["todos"]
	if !ok {
		return operation.Spec{}, fmt.Errorf("todos is required")
	}
	// Pass through raw todos
	return operation.Spec{Type: "todowrite", Args: map[string]any{"todos": todos, "session_id": args["session_id"]}}, nil
}
func (t *TodoWriteTool) TranslateResult(op operation.Operation) string { return op.Output }
