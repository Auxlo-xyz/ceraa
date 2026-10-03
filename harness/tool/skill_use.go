package tool

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/operation"
	"ceraa/harness/skills"
)

// SkillProposalName is the tool that offers a new version of a skill.
const SkillProposalName = "skill_propose"

// SkillUseTool serves one approved skill's body.
//
// This is the other half of RenderCatalog. The catalogue carries a name and a
// description for every approved skill and none of the bodies, so the standing
// cost is a line per skill instead of the whole library on every request. The
// body arrives here, once, for the skill the task actually needs.
//
// Reading from the store rather than from a SKILL.md on disk is the difference
// from the reference design, and it is deliberate: Ceraa's store carries an
// approval state and a version history, and a file on disk would have neither.
// An unapproved proposal is not in the catalogue and not resolvable here, which
// is the property that makes the smaller prompt safe.
type SkillUseTool struct {
	Store *skills.Store
}

func (t *SkillUseTool) Name() string { return "skill_use" }

// Description is the tool's contract and nothing else. Which task should reach
// for it is guidance, and guidance belongs in the prompt's toolrules and in the
// catalogue preamble, not here.
func (t *SkillUseTool) Description() string {
	return "Read the full instructions of one approved skill by the name it is listed under. " +
		"Fails with the list of known names if the skill is not approved."
}

func (t *SkillUseTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "description": "Skill name exactly as listed"},
		},
		"required": []string{"name"},
	}
}

func (t *SkillUseTool) Translate(args map[string]any) (operation.Spec, error) {
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return operation.Spec{}, fmt.Errorf("skill_use requires a skill name")
	}
	// Normalised here rather than at execution so the spec that gets persisted
	// carries the name that was validated, not the spacing that arrived with it.
	return operation.Spec{Type: t.Name(), Args: map[string]any{"name": name}}, nil
}

func (t *SkillUseTool) TranslateResult(op operation.Operation) string { return op.Output }

func (t *SkillUseTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	if t.Store == nil {
		return "", fmt.Errorf("skill store is not configured")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	name, _ := spec.Args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("skill_use requires a skill name")
	}
	skill, err := t.Store.Current(name)
	if err != nil {
		// The catalogue is the list of names that resolve, so a miss is answered
		// with that list rather than with the store's own error. A model that
		// guessed a name should be able to correct it in one more call.
		return "", fmt.Errorf("no approved skill named %q; approved skills are: %s", name, t.knownNames())
	}
	body := skills.CurrentContent(*skill)
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("approved skill %q has no content", name)
	}
	return fmt.Sprintf("# Skill: %s (version %d)\n\n%s", skill.Name, skills.LatestVersion(*skill), body), nil
}

// knownNames lists what is actually available so the error is actionable rather
// than merely accurate.
func (t *SkillUseTool) knownNames() string {
	names, err := t.Store.Names()
	if err != nil || len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
