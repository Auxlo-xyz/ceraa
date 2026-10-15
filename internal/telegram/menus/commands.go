package menus

type CommandSpec struct {
	Command     string
	Description string
}

func Commands() []CommandSpec {
	return []CommandSpec{
		{"start", "Open Ceraa"},
		{"help", "Show available commands"},
		{"keyboard", "Force the reply keyboard to reappear"},
		{"apilog", "Toggle Telegram API call tracing"},
		{"hub", "Open the control hub"},
		{"web", "Share the Mini App link"},
		{"purge", "Mint a fresh web address and republish it"},
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
		{"project", "Choose the workspace"},
		{"tools", "List available harness tools"},
		{"todos", "Show session todos"},
		{"context", "Show context usage"},
		{"cache", "Show cache hit rate by endpoint"},
		{"compaction", "Show or run compaction"},
		{"compaction_level", "Show compaction controls"},
		{"snapshots", "List file snapshots"},
		{"revert", "Restore the latest snapshot"},
		{"permission", "Open permission controls"},
		{"skills_catalog", "Show available tool skills"},
		{"file_browser", "Browse workspace files"},
		{"abort", "Stop the current run (alias: /stop)"},
		{"stop", "Stop the current run"},
		{"restart", "Restart the bot process"},
		{"detach", "Detach from the current session"},
		{"attach", "Attach to the current session"},
		{"worktree", "Show worktree information"},
		{"secrets", "List encrypted secret names"},
		{"setsecret", "Store an encrypted secret"},
		{"delsecret", "Delete an encrypted secret"},
		{"codex-login", "Start Codex device login"},
		{"codex-status", "Show Codex login status"},
		{"hermes-login", "Start Nous Portal device login"},
		{"hermes-status", "Show Hermes login status"},
	}
}
