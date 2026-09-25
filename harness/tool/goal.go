package tool

import (
	"encoding/json"
	"fmt"
	"strings"

	"sooraa/harness/goal"
	"sooraa/harness/operation"
)

type CreateGoalTool struct{}

func (t *CreateGoalTool) Name() string { return "create_goal" }
func (t *CreateGoalTool) Description() string {
	return "Create a persistent active goal for long-running work."
}
func (t *CreateGoalTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"objective":    map[string]any{"type": "string", "minLength": 1},
			"token_budget": map[string]any{"type": "integer", "minimum": 0},
		},
		"required": []string{"objective"},
	}
}
func (t *CreateGoalTool) Translate(args map[string]any) (operation.Spec, error) {
	objective, _ := args["objective"].(string)
	if strings.TrimSpace(objective) == "" {
		return operation.Spec{}, fmt.Errorf("objective is required")
	}
	return operation.Spec{Type: "create_goal", Args: map[string]any{"objective": objective, "token_budget": args["token_budget"]}}, nil
}
func (t *CreateGoalTool) TranslateResult(op operation.Operation) string { return op.Output }

func (t *CreateGoalTool) Run(spec operation.Spec) (string, error) {
	return runGoal(spec, "create")
}

type GetGoalTool struct{}

func (t *GetGoalTool) Name() string        { return "get_goal" }
func (t *GetGoalTool) Description() string { return "Read the current persistent goal state." }
func (t *GetGoalTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *GetGoalTool) Translate(args map[string]any) (operation.Spec, error) {
	return operation.Spec{Type: "get_goal", Args: map[string]any{}}, nil
}
func (t *GetGoalTool) TranslateResult(op operation.Operation) string { return op.Output }
func (t *GetGoalTool) Run(spec operation.Spec) (string, error) {
	return runGoal(spec, "get")
}

type UpdateGoalTool struct{}

func (t *UpdateGoalTool) Name() string        { return "update_goal" }
func (t *UpdateGoalTool) Description() string { return "Update the persistent goal status or reason." }
func (t *UpdateGoalTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{"type": "string", "enum": []string{"active", "paused", "completed", "blocked"}},
			"reason": map[string]any{"type": "string"},
		},
		"required": []string{"status"},
	}
}
func (t *UpdateGoalTool) Translate(args map[string]any) (operation.Spec, error) {
	status, _ := args["status"].(string)
	if status == "" {
		return operation.Spec{}, fmt.Errorf("status is required")
	}
	return operation.Spec{Type: "update_goal", Args: map[string]any{"status": status, "reason": args["reason"]}}, nil
}
func (t *UpdateGoalTool) TranslateResult(op operation.Operation) string { return op.Output }
func (t *UpdateGoalTool) Run(spec operation.Spec) (string, error) {
	return runGoal(spec, "update")
}

func runGoal(spec operation.Spec, action string) (string, error) {
	dir, _ := spec.Args["goal_dir"].(string)
	store, err := goal.New(dir)
	if err != nil {
		return "", err
	}
	sessionID, _ := spec.Args["session_id"].(string)
	if sessionID == "" {
		sessionID = "default"
	}
	current, exists, err := store.Load(sessionID)
	if err != nil {
		return "", err
	}
	switch action {
	case "create":
		objective, _ := spec.Args["objective"].(string)
		budget := 0
		switch value := spec.Args["token_budget"].(type) {
		case int:
			budget = value
		case int64:
			budget = int(value)
		case float64:
			budget = int(value)
		}
		if exists && current.Status == goal.StatusActive {
			current.Objective = objective
			current.TokenBudget = budget
			current.Status = goal.StatusActive
			current.Reason = ""
		} else {
			current = goal.Goal{Objective: objective, Status: goal.StatusActive, TokenBudget: budget}
		}
		if err := store.Save(sessionID, current); err != nil {
			return "", err
		}
		return encodeGoal(current), nil
	case "get":
		if !exists {
			return "No persistent goal exists.", nil
		}
		return encodeGoal(current), nil
	case "update":
		if !exists {
			return "", fmt.Errorf("no persistent goal exists")
		}
		status, _ := spec.Args["status"].(string)
		current.Status = goal.Status(status)
		current.Reason, _ = spec.Args["reason"].(string)
		if err := store.Save(sessionID, current); err != nil {
			return "", err
		}
		return encodeGoal(current), nil
	default:
		return "", fmt.Errorf("unknown goal action")
	}
}

func encodeGoal(value goal.Goal) string {
	data, _ := json.Marshal(value)
	return string(data)
}
