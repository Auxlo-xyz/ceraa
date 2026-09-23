package menus

import (
	"fmt"
	"os"
	"path/filepath"

	"sooraa/harness/sessionstore"
	"sooraa/internal/config"
)

var All = []string{
	"project", "worktree", "agent", "variant", "model",
	"session", "scheduled_task", "message_history", "file_browser",
	"settings", "permission", "context_control", "compaction_level", "mcp_catalog", "skills_catalog", "rename", "question", "command_catalog",
}

func Inline(name string, page int) map[string]any {
	_ = page
	switch name {
	case "model":
		return modelMenu()
	case "agent":
		return agentMenu()
	case "session":
		return sessionMenu()
	case "file_browser":
		return fileBrowserMenu(".")
	case "project":
		return projectMenu()
	case "settings":
		return settingsMenu()
	case "message_history":
		return messageHistoryMenu()
	case "worktree":
		return worktreeMenu()
	case "variant":
		return variantMenu()
	case "permission":
		return permissionMenu()
	case "context_control":
		return contextControlMenu()
	case "compaction_level":
		return compactionMenu()
	case "mcp_catalog", "skills_catalog", "command_catalog":
		return catalogMenu(name)
	case "scheduled_task":
		return scheduledTaskMenu()
	case "rename", "question":
		return simpleMenu(name, "Use /"+name+" <args> command")
	default:
		rows := [][]map[string]string{
			{{"text": "✕ Close", "callback_data": "close"}},
		}
		return map[string]any{"inline_keyboard": rows}
	}
}

func modelMenu() map[string]any {
	cfg, _ := config.Load()
	current := ""
	if cfg != nil {
		current = cfg.LLM.Model
	}
	models := []string{"poolside/laguna-xs-2.1", "atria/dawn-preview", "openai/gpt-4o", "anthropic/claude-3.5-sonnet"}
	var rows [][]map[string]string
	for _, m := range models {
		label := m
		if m == current {
			label = "✓ " + m
		}
		rows = append(rows, []map[string]string{{"text": label, "callback_data": "select_model:" + m}})
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func agentMenu() map[string]any {
	agents := []string{"plan", "build"}
	rows := [][]map[string]string{}
	for _, a := range agents {
		rows = append(rows, []map[string]string{{"text": a, "callback_data": "select_agent:" + a}})
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func sessionMenu() map[string]any {
	store, err := sessionstore.New("sessions")
	if err != nil {
		return simpleMenu("session", "No sessions")
	}
	ids, err := store.List()
	if err != nil || len(ids) == 0 {
		return simpleMenu("session", "No sessions yet")
	}
	var rows [][]map[string]string
	for i, id := range ids {
		if i >= 8 {
			rows = append(rows, []map[string]string{{"text": fmt.Sprintf("... %d more", len(ids)-8), "callback_data": "close"}})
			break
		}
		label := id
		if len(label) > 24 {
			label = label[:24] + "..."
		}
		rows = append(rows, []map[string]string{{"text": label, "callback_data": "open_session:" + id}})
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func fileBrowserMenu(path string) map[string]any {
	entries, err := os.ReadDir(path)
	if err != nil {
		return simpleMenu("file_browser", "Cannot read: "+path)
	}
	var rows [][]map[string]string
	for i, e := range entries {
		if i >= 8 {
			break
		}
		name := e.Name()
		if len(name) > 20 {
			name = name[:20] + "..."
		}
		if e.IsDir() {
			name += "/"
		}
		rows = append(rows, []map[string]string{{"text": name, "callback_data": "browse:" + filepath.Join(path, e.Name())}})
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func projectMenu() map[string]any {
	cwd, _ := os.Getwd()
	rows := [][]map[string]string{
		{{"text": cwd, "callback_data": "close"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}
	return map[string]any{"inline_keyboard": rows}
}

func settingsMenu() map[string]any {
	cfg, _ := config.Load()
	model := "unknown"
	if cfg != nil {
		model = cfg.LLM.Model
	}
	rows := [][]map[string]string{
		{{"text": "Model: " + model, "callback_data": "menu:model"}},
		{{"text": "Max steps: " + fmt.Sprint(cfg.MaxSteps), "callback_data": "close"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}
	return map[string]any{"inline_keyboard": rows}
}

func messageHistoryMenu() map[string]any {
	rows := [][]map[string]string{
		{{"text": "Show last 5", "callback_data": "show_messages:5"}},
		{{"text": "Show last 20", "callback_data": "show_messages:20"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}
	return map[string]any{"inline_keyboard": rows}
}

func worktreeMenu() map[string]any {
	rows := [][]map[string]string{
		{{"text": "git worktree list", "callback_data": "worktree:list"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}
	return map[string]any{"inline_keyboard": rows}
}

func variantMenu() map[string]any {
	rows := [][]map[string]string{
		{{"text": "default", "callback_data": "select_variant:default"}},
		{{"text": "compact", "callback_data": "select_variant:compact"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}
	return map[string]any{"inline_keyboard": rows}
}

func permissionMenu() map[string]any {
	rows := [][]map[string]string{
		{{"text": "allowOnce", "callback_data": "permission:allowOnce"}},
		{{"text": "allowAlways", "callback_data": "permission:allowAlways"}},
		{{"text": "deny", "callback_data": "permission:deny"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}
	return map[string]any{"inline_keyboard": rows}
}

func contextControlMenu() map[string]any {
	rows := [][]map[string]string{
		{{"text": "Clear context", "callback_data": "context:clear"}},
		{{"text": "Show context", "callback_data": "context:show"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}
	return map[string]any{"inline_keyboard": rows}
}

func compactionMenu() map[string]any {
	levels := []string{"auto", "low", "medium", "high"}
	var rows [][]map[string]string
	for _, l := range levels {
		rows = append(rows, []map[string]string{{"text": l, "callback_data": "compaction:" + l}})
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func catalogMenu(name string) map[string]any {
	return simpleMenu(name, "Catalog "+name+" not yet wired - see harness/tool/registry.go")
}

func scheduledTaskMenu() map[string]any {
	rows := [][]map[string]string{
		{{"text": "List tasks", "callback_data": "task:list"}},
		{{"text": "Create task", "callback_data": "task:create"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}
	return map[string]any{"inline_keyboard": rows}
}

func simpleMenu(name, msg string) map[string]any {
	rows := [][]map[string]string{
		{{"text": msg, "callback_data": "close"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}
	_ = name
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
