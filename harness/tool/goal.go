package tool

import (
	"encoding/json"
	"fmt"
	"strings"

	"ceraa/harness/goal"
	"ceraa/harness/operation"
	"ceraa/harness/todo"
)

type CreateGoalTool struct{}

func (t *CreateGoalTool) Name() string { return "create_goal" }
func (t *CreateGoalTool) Description() string {
	return "Create a persistent active goal."
}
func (t *CreateGoalTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"objective": map[string]any{"type": "string", "minLength": 1},
		},
		"required": []string{"objective"},
	}
}
func (t *CreateGoalTool) Translate(args map[string]any) (operation.Spec, error) {
	objective, _ := args["objective"].(string)
	if strings.TrimSpace(objective) == "" {
		return operation.Spec{}, fmt.Errorf("objective is required")
	}
	return operation.Spec{Type: "create_goal", Args: map[string]any{"objective": objective}}, nil
}
func (t *CreateGoalTool) TranslateResult(op operation.Operation) string { return op.Output }

func (t *CreateGoalTool) Run(spec operation.Spec) (string, error) {
	return runGoal(spec, "create")
}

type GetGoalTool struct{}

func (t *GetGoalTool) Name() string        { return "get_goal" }
func (t *GetGoalTool) Description() string { return "Read the current persistent goal state." }
func (t *GetGoalTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *GetGoalTool) Translate(args map[string]any) (operation.Spec, error) {
	return operation.Spec{Type: "get_goal", Args: map[string]any{}}, nil
}
func (t *GetGoalTool) TranslateResult(op operation.Operation) string { return op.Output }
func (t *GetGoalTool) Run(spec operation.Spec) (string, error) {
	return runGoal(spec, "get")
}

type UpdateGoalTool struct{}

func (t *UpdateGoalTool) Name() string { return "update_goal" }
func (t *UpdateGoalTool) Description() string {
	return "Update the persistent goal status, reason, or plan. Plan items live on the goal, so get_goal reads back what this writes."
}
func (t *UpdateGoalTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{"type": "string", "enum": []string{"active", "paused", "completed", "blocked"}},
			"reason": map[string]any{"type": "string"},
			"todos": map[string]any{
				"type":        "array",
				"description": "Replaces the plan.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content":  map[string]any{"type": "string"},
						"status":   map[string]any{"type": "string", "enum": []string{goal.TodoPending, goal.TodoInProgress, goal.TodoCompleted, goal.TodoCancelled}},
						"priority": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
					},
					"required": []string{"content", "status", "priority"},
				},
			},
		},
	}
}
func (t *UpdateGoalTool) Translate(args map[string]any) (operation.Spec, error) {
	status, _ := args["status"].(string)
	todos, hasTodos := args["todos"]
	// A plan-only update is the common call now that the plan lives here, so
	// status cannot stay required. Requiring one field still rules out a call
	// that would change nothing and answer with a silent success.
	if status == "" && !hasTodos {
		return operation.Spec{}, fmt.Errorf("status or todos is required")
	}
	spec := operation.Spec{Type: "update_goal", Args: map[string]any{"status": status, "reason": args["reason"]}}
	if hasTodos {
		spec.Args["todos"] = todos
	}
	return spec, nil
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
	// The record is the owner's one goal, shared by every runner and every
	// surface; the spec's session id is used only for the legacy sidecar
	// adoption below, which addresses the per-session plan files the goal once
	// lived in.
	current, exists, err := store.Load(goal.GlobalKey)
	if err != nil {
		return "", err
	}
	// The dashboard reads the plan through todo.Store, which still adopts the
	// pre-merge sidecar file. Adopting here too is what keeps get_goal and the
	// dashboard from showing two different plans for one session. Best effort: the
	// goal is the record of authority, so a sidecar that cannot be read is left
	// alone rather than failing a read of the goal.
	if exists && len(current.Todos) == 0 {
		if plan, planErr := todo.New(dir); planErr == nil {
			if legacy, loadErr := plan.Load(sessionID); loadErr == nil && len(legacy) > 0 {
				current.Todos = legacy
			}
		}
	}
	switch action {
	case "create":
		objective, _ := spec.Args["objective"].(string)
		// The plan belongs to the objective it was written for. Re-declaring
		// the same objective keeps it, and a plan no objective has claimed yet
		// belongs to whatever comes next. A genuinely new objective must not
		// inherit the old one's checklist.
		previousObjective := ""
		var previousPlan []goal.Todo
		if exists {
			previousObjective = strings.TrimSpace(current.Objective)
			previousPlan = current.Todos
		}
		if exists && current.Status == goal.StatusActive {
			current.Objective = objective
			current.Status = goal.StatusActive
			current.Reason = ""
		} else {
			current = goal.Goal{Objective: objective, Status: goal.StatusActive}
		}
		if previousObjective == "" || previousObjective == strings.TrimSpace(objective) {
			current.Todos = previousPlan
		} else {
			current.Todos = nil
		}
		saved, err := store.Save(goal.GlobalKey, current)
		if err != nil {
			return "", err
		}
		return encodeGoal(saved), nil
	case "get":
		if !exists {
			return "No persistent goal exists.", nil
		}
		// A record with no objective is a plan written before any objective
		// existed (todowrite creates the record to carry the plan). Answering
		// with it raw reads as an active goal with an empty objective, which is
		// not what it is: there is a plan and no goal yet.
		if strings.TrimSpace(current.Objective) == "" {
			if len(current.Todos) == 0 {
				return "No goal has been set yet (call create_goal to set one). No plan yet (todowrite writes one).", nil
			}
			return fmt.Sprintf("No goal has been set yet (call create_goal to set one).\nCurrent plan (%d items):\n%s",
				len(current.Todos), goal.Render(current.Todos)), nil
		}
		return encodeGoal(current), nil
	case "update":
		if !exists {
			return "", fmt.Errorf("no persistent goal exists")
		}
		// A status change owns its reason: the call that moves the goal states
		// why, and one that says nothing means there is no longer one. A
		// plan-only update is not about the reason at all and must not clear it.
		if status, ok := spec.Args["status"].(string); ok && status != "" {
			current.Status = goal.Status(status)
			current.Reason, _ = spec.Args["reason"].(string)
		} else if reason, ok := spec.Args["reason"].(string); ok && reason != "" {
			current.Reason = reason
		}
		if raw, ok := spec.Args["todos"]; ok {
			todos, err := goal.ParseTodos(raw)
			if err != nil {
				return "", fmt.Errorf("todos: %w", err)
			}
			current.Todos = todos
		}
		saved, err := store.Save(goal.GlobalKey, current)
		if err != nil {
			return "", err
		}
		return encodeGoal(saved), nil
	default:
		return "", fmt.Errorf("unknown goal action")
	}
}

func encodeGoal(value goal.Goal) string {
	data, _ := json.Marshal(value)
	return string(data)
}
