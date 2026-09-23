package menus

import _ "fmt"

var All = []string{
	"project", "worktree", "agent", "variant", "model",
	"session", "scheduled_task", "message_history", "file_browser",
	"settings", "permission", "context_control", "compaction_level", "mcp_catalog", "skills_catalog", "rename", "question", "command_catalog",
}

func Inline(name string, page int) map[string]any {
	// No placeholder data - real catalog to be wired per-menu.
	// Until then show empty state + Close button, no fake "name 1/2/3" rows.
	_ = page
	rows := [][]map[string]string{
		{{"text": "✕ Close", "callback_data": "close"}},
	}
	return map[string]any{"inline_keyboard": rows}
}

func Hub() map[string]any {
	rows := [][]map[string]string{}
	for i, n := range All {
		if i%2 == 0 {
			rows = append(rows, []map[string]string{{"text": n, "callback_data": "menu:" + n}})
		} else {
			rows[len(rows)-1] = append(rows[len(rows)-1], map[string]string{"text": n, "callback_data": "menu:" + n})
		}
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}
