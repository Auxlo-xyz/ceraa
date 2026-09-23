package tool

import (
	"fmt"
	"strings"

	"sooraa/harness/operation"
)

// SkillsCatalogTool lists available skills, maps to soraacode skill/discovery.ts + skill/guidance.ts
type SkillsCatalogTool struct{}

func (s *SkillsCatalogTool) Name() string { return "skills_catalog" }
func (s *SkillsCatalogTool) Description() string {
	return "List available skills. Skills are reusable workflows like file editing, search, etc."
}
func (s *SkillsCatalogTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "Filter skills by name"},
		},
	}
}
func (s *SkillsCatalogTool) Translate(args map[string]any) (operation.Spec, error) {
	q, _ := args["query"].(string)
	return operation.Spec{Type: "skills_catalog", Args: map[string]any{"query": q}}, nil
}
func (s *SkillsCatalogTool) TranslateResult(op operation.Operation) string { return op.Output }

type CommandCatalogTool struct{}

func (c *CommandCatalogTool) Name() string { return "command_catalog" }
func (c *CommandCatalogTool) Description() string {
	return "List available slash commands and their descriptions."
}
func (c *CommandCatalogTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "Filter commands"},
		},
	}
}
func (c *CommandCatalogTool) Translate(args map[string]any) (operation.Spec, error) {
	q, _ := args["query"].(string)
	return operation.Spec{Type: "command_catalog", Args: map[string]any{"query": q}}, nil
}
func (c *CommandCatalogTool) TranslateResult(op operation.Operation) string { return op.Output }

type MCPCatalogTool struct{}

func (m *MCPCatalogTool) Name() string { return "mcp_catalog" }
func (m *MCPCatalogTool) Description() string {
	return "List configured MCP servers and their tools."
}
func (m *MCPCatalogTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "Filter MCP servers"},
		},
	}
}
func (m *MCPCatalogTool) Translate(args map[string]any) (operation.Spec, error) {
	q, _ := args["query"].(string)
	return operation.Spec{Type: "mcp_catalog", Args: map[string]any{"query": q}}, nil
}
func (m *MCPCatalogTool) TranslateResult(op operation.Operation) string { return op.Output }

// CatalogRunner handles all three catalog types
type CatalogRunner struct{}

func (r *CatalogRunner) Run(spec operation.Spec) (string, error) {
	query, _ := spec.Args["query"].(string)
	lowerQ := strings.ToLower(query)
	switch spec.Type {
	case "skills_catalog":
		skills := []struct{ Name, Desc string }{
			{"read", "Read file contents or list directory"},
			{"write", "Create or overwrite file"},
			{"bash", "Execute shell command"},
			{"grep", "Search via ripgrep"},
			{"glob", "File pattern match"},
			{"fts_search", "Indexed full-text search via FTS5"},
			{"question", "Ask user with buttons"},
			{"webfetch", "Fetch URL content"},
			{"websearch", "Search web via Exa"},
			{"view_image", "View image file"},
		}
		var out strings.Builder
		for _, s := range skills {
			if lowerQ != "" && !strings.Contains(strings.ToLower(s.Name), lowerQ) && !strings.Contains(strings.ToLower(s.Desc), lowerQ) {
				continue
			}
			fmt.Fprintf(&out, "• %s: %s\n", s.Name, s.Desc)
		}
		if out.Len() == 0 {
			return "No skills match: " + query, nil
		}
		return out.String(), nil
	case "command_catalog":
		cmds := []struct{ Name, Desc string }{
			{"/new", "New session"},
			{"/sessions", "List sessions"},
			{"/open", "Open session"},
			{"/messages", "Show recent messages"},
			{"/ls", "List files"},
			{"/status", "Show harness status"},
			{"/settings", "Show config"},
			{"/model", "Select model"},
			{"/agent", "Select agent"},
			{"/project", "Project menu"},
			{"/worktree", "Git worktree"},
			{"/compaction", "Compaction status"},
			{"/help", "Full help"},
			{"/abort", "Abort turn"},
			{"/setsecret", "Store secret"},
		}
		var out strings.Builder
		for _, c := range cmds {
			if lowerQ != "" && !strings.Contains(strings.ToLower(c.Name), lowerQ) {
				continue
			}
			fmt.Fprintf(&out, "• %s: %s\n", c.Name, c.Desc)
		}
		return out.String(), nil
	case "mcp_catalog":
		// Sooraa has no MCP servers yet, maps to soraacode config/mcp.ts
		return "No MCP servers configured. Add to config.json mcp section. (soraacode has mcp-websearch, etc.)", nil
	}
	return "", fmt.Errorf("unknown catalog type: %s", spec.Type)
}
