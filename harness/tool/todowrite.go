package tool

import (
	"encoding/json"
	"fmt"

	"sooraa/harness/operation"
	"sooraa/harness/todo"
)

// TodoWriteTool maps to soraacode tool/todowrite.ts persisted TodoWrite
type TodoWriteTool struct{}

func (t *TodoWriteTool) Name() string { return "todowrite" }
func (t *TodoWriteTool) Description() string {
	return "Manage todo list for the session. Use to track progress."
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

// TodoWriteRunner handles todowrite operations
type TodoWriteRunner struct {
	Dir string
}

func (r *TodoWriteRunner) Run(spec operation.Spec) (string, error) {
	sessionID, _ := spec.Args["session_id"].(string)
	if sessionID == "" {
		sessionID = "default"
	}
	dir := r.Dir
	if dir == "" {
		dir = "sessions"
	}
	store, err := todo.New(dir)
	if err != nil {
		return "", err
	}
	// Parse todos
	raw, _ := json.Marshal(spec.Args["todos"])
	var todos []todo.Todo
	if err := json.Unmarshal(raw, &todos); err != nil {
		return "", err
	}
	if err := store.Save(sessionID, todos); err != nil {
		return "", err
	}
	return fmt.Sprintf("Todos updated: %d items", len(todos)), nil
}
