package menus

type CommandSpec struct {
	Command     string
	Description string
}

func Commands() []CommandSpec {
	return []CommandSpec{
		{"start", "Open Sooraa"},
		{"help", "Show available commands"},
		{"hub", "Open the control hub"},
		{"new", "Start a fresh session"},
		{"sessions", "List stored sessions"},
		{"session", "Open session controls"},
		{"open", "Attach to a session"},
		{"messages", "Show recent messages"},
		{"ls", "List workspace files"},
		{"delete", "Delete a session"},
		{"rename", "Rename a session"},
		{"status", "Show live harness status"},
		{"steps", "Set agent step limit"},
		{"settings", "Show safe settings"},
		{"model", "Choose the model"},
		{"agent", "Choose the agent mode"},
		{"variant", "Choose the response variant"},
		{"project", "Choose the workspace"},
		{"tools", "List available harness tools"},
		{"todos", "Show session todos"},
		{"context", "Show context usage"},
		{"compaction", "Show or run compaction"},
		{"compaction_level", "Show compaction controls"},
		{"search", "Search the workspace index"},
		{"index", "Index a workspace"},
		{"snapshots", "List file snapshots"},
		{"revert", "Restore the latest snapshot"},
		{"permission", "Open permission controls"},
		{"skills_catalog", "Show available tool skills"},
		{"command_catalog", "Show bot commands"},
		{"mcp_catalog", "Show MCP servers"},
		{"file_browser", "Browse workspace files"},
		{"abort", "Stop the current run"},
		{"detach", "Detach from the current session"},
		{"attach", "Attach to the current session"},
		{"worktree", "Show worktree information"},
		{"secrets", "List encrypted secret names"},
		{"setsecret", "Store an encrypted secret"},
		{"delsecret", "Delete an encrypted secret"},
		{"codex-login", "Start Codex device login"},
		{"codex-status", "Show Codex login status"},
	}
}
