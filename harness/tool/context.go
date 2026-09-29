package tool

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/skills"
)

type MemoryTool struct {
	Store *memory.Store
}

func (m *MemoryTool) Name() string { return "memory" }
func (m *MemoryTool) Description() string {
	return "Store or retrieve compact project and session memory. Use for durable preferences, decisions, and unfinished work, not full transcripts. Memories fade unless re-asserted, so confirm one that still holds and contradict one the user has reversed."
}
func (m *MemoryTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":  map[string]any{"type": "string", "enum": []string{"add", "confirm", "contradict", "list", "remove"}},
			"content": map[string]any{"type": "string", "description": "Compact memory to add"},
			"id":      map[string]any{"type": "string", "description": "Memory ID to remove"},
		},
		"required": []string{"action"},
	}
}
func (m *MemoryTool) Translate(args map[string]any) (operation.Spec, error) {
	action, _ := args["action"].(string)
	if action != "add" && action != "confirm" && action != "contradict" && action != "list" && action != "remove" {
		return operation.Spec{}, fmt.Errorf("memory action must be add, confirm, contradict, list, or remove")
	}
	return operation.Spec{Type: "memory", Args: args}, nil
}
func (m *MemoryTool) TranslateResult(op operation.Operation) string { return op.Output }
func (m *MemoryTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	if m.Store == nil {
		return "", fmt.Errorf("memory store is not configured")
	}
	project, _ := spec.Args["workdir"].(string)
	if project == "" {
		project = "default"
	}
	sessionID, _ := spec.Args["session_id"].(string)
	switch spec.Args["action"] {
	case "add":
		content, _ := spec.Args["content"].(string)
		entry, err := m.Store.Add(project, sessionID, content)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Memory saved: %s", entry.ID), nil
	case "confirm":
		// The id of an existing memory that still holds. Add is the assertion
		// path and dedups on content, so confirm and add differ only in intent:
		// confirm restates something already known and must not invent wording
		// that misses the exact-content match and creates a second entry.
		id, _ := spec.Args["id"].(string)
		if strings.TrimSpace(id) == "" {
			return "", fmt.Errorf("confirm needs the id of an existing memory")
		}
		if err := m.Store.Confirm(project, sessionID, id); err != nil {
			return "", err
		}
		return "Memory confirmed: " + id, nil
	case "contradict":
		// The user has reversed this. Removing it is the whole point: leaving it
		// behind leaves a preference that is still being rendered and still reads
		// as current.
		id, _ := spec.Args["id"].(string)
		if strings.TrimSpace(id) == "" {
			return "", fmt.Errorf("contradict needs the id of an existing memory")
		}
		if err := m.Store.Remove(id); err != nil {
			return "", err
		}
		return "Memory contradicted and dropped: " + id, nil
	case "list":
		return m.Store.RenderContext(project, sessionID, 3000)
	case "remove":
		id, _ := spec.Args["id"].(string)
		if err := m.Store.Remove(id); err != nil {
			return "", err
		}
		return "Memory removed: " + id, nil
	default:
		return "", fmt.Errorf("unsupported memory action")
	}
}

type SkillProposalTool struct {
	Store *skills.Store
}

func (s *SkillProposalTool) Name() string { return "skill_propose" }
func (s *SkillProposalTool) Description() string {
	return "Create a proposed version of a reusable skill from user feedback. This never changes the active skill until a person approves the proposal."
}
func (s *SkillProposalTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"skill_name": map[string]any{"type": "string", "description": "Stable skill name"},
			"content":    map[string]any{"type": "string", "description": "Complete proposed skill content"},
			"feedback":   map[string]any{"type": "string", "description": "User feedback or failure that motivated the change"},
		},
		"required": []string{"skill_name", "content", "feedback"},
	}
}
func (s *SkillProposalTool) Translate(args map[string]any) (operation.Spec, error) {
	return operation.Spec{Type: "skill_propose", Args: args}, nil
}
func (s *SkillProposalTool) TranslateResult(op operation.Operation) string { return op.Output }
func (s *SkillProposalTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	if s.Store == nil {
		return "", fmt.Errorf("skill store is not configured")
	}
	name, _ := spec.Args["skill_name"].(string)
	content, _ := spec.Args["content"].(string)
	feedback, _ := spec.Args["feedback"].(string)
	proposal, err := s.Store.Propose(name, content, feedback)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Skill proposal %s created for %s; approval is required before activation.", proposal.ID, proposal.SkillName), nil
}
