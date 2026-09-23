package menus

import "fmt"

var All = []string{
	"project", "worktree", "agent", "variant", "model",
	"session", "scheduled_task", "message_history", "file_browser",
	"settings", "permission", "context_control", "compaction_level", "mcp_catalog", "skills_catalog", "rename", "question", "command_catalog",
}

func Inline(name string, page int) map[string]any {
	rows := [][]map[string]string{}
	for i := 0; i < 5; i++ {
		idx := page*5 + i + 1
		rows = append(rows, []map[string]string{{"text": fmt.Sprintf("%s %d", name, idx), "callback_data": fmt.Sprintf("%s:%d", name, idx)}})
	}
	rows = append(rows, []map[string]string{
		{"text": "◀", "callback_data": fmt.Sprintf("%s:page:%d", name, page-1)},
		{"text": "▶", "callback_data": fmt.Sprintf("%s:page:%d", name, page+1)},
	})
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
	return map[string]any{"inline_keyboard": rows}
}
