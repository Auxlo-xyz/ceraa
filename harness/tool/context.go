package tool

import (
	"context"
	"fmt"

	"ceraa/harness/operation"
	"ceraa/harness/skills"
)

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
