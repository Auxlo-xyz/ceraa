package menus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"sooraa/harness/sessionstore"
	"sooraa/harness/todo"
	"sooraa/harness/tool"
	"sooraa/internal/config"
)

var All = []string{
	"status", "session", "model", "agent", "variant", "project", "tools", "todos",
	"context", "compaction_level", "file_browser", "settings", "permission", "search",
	"index", "snapshots", "message_history", "skills_catalog", "command_catalog", "mcp_catalog", "worktree",
}

type runtimeState struct {
	Model          string            `json:"model,omitempty"`
	Agent          string            `json:"agent,omitempty"`
	Variant        string            `json:"variant,omitempty"`
	MaxSteps       int               `json:"max_steps,omitempty"`
	AutoCompaction bool              `json:"auto_compaction,omitempty"`
	AutoSet        bool              `json:"auto_compaction_set,omitempty"`
	Projects       map[string]string `json:"projects,omitempty"`
}

func runtimeValues() runtimeState {
	home, _ := os.UserHomeDir()
	data, _ := os.ReadFile(filepath.Join(home, ".config", "sooraa", "bridge-state.json"))
	var state runtimeState
	_ = json.Unmarshal(data, &state)
	return state
}

func projectForChat(chatID int64) string {
	state := runtimeValues()
	if root := state.Projects[strconv.FormatInt(chatID, 10)]; root != "" {
		return root
	}
	wd, _ := os.Getwd()
	return wd
}

func configuredStore() (*sessionstore.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return sessionstore.New(cfg.SessionFile)
}

func InlinePath(path string) map[string]any {
	return fileBrowserMenu(path)
}

func Inline(name string, page int) map[string]any {
	return InlineForSession(name, page, "")
}

func InlineForChat(name string, page int, chatID int64, sessionID string) map[string]any {
	root := projectForChat(chatID)
	switch name {
	case "project":
		return projectMenu(root)
	case "file_browser":
		return fileBrowserMenu(root)
	default:
		return InlineForSession(name, page, sessionID)
	}
}

func InlineForSession(name string, page int, sessionID string) map[string]any {
	_ = page
	switch name {
	case "status", "context":
		return statusMenu(sessionID)
	case "model":
		return modelMenu()
	case "agent":
		return agentMenu()
	case "variant":
		return variantMenu()
	case "session":
		return sessionMenu(sessionID)
	case "file_browser":
		return fileBrowserMenu(".")
	case "project":
		return projectMenu()
	case "tools", "skills_catalog":
		return toolsMenu()
	case "todos":
		return todosMenu(sessionID)
	case "settings":
		return settingsMenu()
	case "permission":
		return permissionMenu()
	case "compaction_level":
		return compactionMenu()
	case "mcp_catalog":
		return simpleMenu("MCP", "No MCP servers configured")
	case "command_catalog":
		return commandMenu()
	case "index":
		return indexMenu()
	case "snapshots":
		return snapshotMenu()
	case "search":
		return searchMenu()
	case "message_history":
		return historyMenu()
	case "worktree":
		return worktreeMenu()
	default:
		return closeMenu()
	}
}

func closeMenu() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "✕ Close", "callback_data": "close"}}}}
}

func modelMenu() map[string]any {
	cfg, _ := config.Load()
	model := "unknown"
	if cfg != nil {
		model = cfg.LLM.Model
	}
	rows := [][]map[string]string{{{"text": "✅ " + model, "callback_data": "noop"}, {"text": "✏️ Set model", "callback_data": "model:custom"}}}
	return map[string]any{"inline_keyboard": rows}
}

func agentMenu() map[string]any {
	state := runtimeValues()
	agent := state.Agent
	if agent == "" {
		agent = "build"
	}
	rows := [][]map[string]string{}
	for _, value := range []string{"build", "plan"} {
		label := value
		if value == agent {
			label = "✅ " + value
		}
		rows = append(rows, []map[string]string{{"text": label, "callback_data": "select_agent:" + value}})
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func variantMenu() map[string]any {
	variant := runtimeValues().Variant
	if variant == "" {
		variant = "default"
	}
	rows := [][]map[string]string{}
	for _, value := range []string{"default", "compact"} {
		label := value
		if value == variant {
			label = "✅ " + value
		}
		rows = append(rows, []map[string]string{{"text": label, "callback_data": "select_variant:" + value}})
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func sessionMenu(active string) map[string]any {
	store, err := configuredStore()
	if err != nil {
		return simpleMenu("Sessions", "Session store unavailable")
	}
	ids, _ := store.List()
	var rows [][]map[string]string
	shown := 0
	for _, id := range ids {
		sess, err := store.Load(id)
		if err != nil || sess.ID != id || id == "inbox" {
			continue
		}
		if shown >= 10 {
			break
		}
		label := fmt.Sprintf("%s · %d msgs", id, len(sess.Messages))
		if id == active {
			label = "✅ " + label
		}
		rows = append(rows, []map[string]string{{"text": label, "callback_data": "attach_session:" + id}})
		shown++
	}
	if len(rows) == 0 {
		return simpleMenu("Sessions", "No sessions yet")
	}
	rows = append(rows, []map[string]string{{"text": "➕ New session", "callback_data": "new_session"}, {"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func fileBrowserMenu(path string) map[string]any {
	entries, err := os.ReadDir(path)
	if err != nil {
		return simpleMenu("Files", "Cannot read "+path)
	}
	rows := [][]map[string]string{}
	for i, entry := range entries {
		if i >= 10 {
			break
		}
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		rows = append(rows, []map[string]string{{"text": name, "callback_data": "browse:" + filepath.Join(path, entry.Name())}})
	}
	if path != "." {
		rows = append(rows, []map[string]string{{"text": "⬆️ Up", "callback_data": "browse:" + filepath.Dir(path)}})
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func projectMenu(roots ...string) map[string]any {
	root := "."
	if len(roots) > 0 && roots[0] != "" {
		root = roots[0]
	}
	rows := [][]map[string]string{}
	entries, _ := os.ReadDir(root)
	shown := 0
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || shown >= 8 {
			continue
		}
		rows = append(rows, []map[string]string{{"text": "📁 " + entry.Name(), "callback_data": "project:" + filepath.Join(root, entry.Name())}})
		shown++
	}
	if len(rows) == 0 {
		return simpleMenu("Project", "Current: "+root)
	}
	rows = append(rows, []map[string]string{{"text": "📍 Current", "callback_data": "project:" + root}, {"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func settingsMenu() map[string]any {
	cfg, _ := config.Load()
	model, url, steps, limit := "unknown", "", 0, 0
	if cfg != nil {
		model, url, steps, limit = cfg.LLM.Model, cfg.LLM.URL, cfg.MaxSteps, cfg.LLM.ContextLimit
	}
	rows := [][]map[string]string{
		{{"text": "🧠 " + model, "callback_data": "menu:model"}},
		{{"text": "🔁 Steps: " + strconv.Itoa(steps), "callback_data": "setting:steps"}},
		{{"text": "🧮 Context: " + strconv.Itoa(limit), "callback_data": "menu:context"}},
		{{"text": "🌐 " + url, "callback_data": "noop"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}
	return map[string]any{"inline_keyboard": rows}
}

func statusMenu(sessionID string) map[string]any {
	_ = sessionID
	rows := [][]map[string]string{
		{{"text": "🗜️ Compact now", "callback_data": "compact:now"}},
		{{"text": "🧹 Clear context", "callback_data": "context:clear"}},
		{{"text": "💬 Recent messages", "callback_data": "menu:message_history"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}
	return map[string]any{"inline_keyboard": rows}
}

func toolsMenu() map[string]any {
	rows := [][]map[string]string{}
	for _, registered := range tool.DefaultRegistry().All() {
		rows = append(rows, []map[string]string{{"text": "🔧 " + registered.Name(), "callback_data": "tool:" + registered.Name()}})
		if len(rows) >= 12 {
			break
		}
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func todosMenu(sessionID string) map[string]any {
	if sessionID == "" {
		return simpleMenu("Todos", "No active session")
	}
	cfg, _ := config.Load()
	if cfg == nil {
		return simpleMenu("Todos", "Config unavailable")
	}
	store, err := todo.New(cfg.SessionFile)
	if err != nil {
		return simpleMenu("Todos", err.Error())
	}
	items, _ := store.Load(sessionID)
	rows := [][]map[string]string{}
	for i, item := range items {
		if i >= 10 {
			break
		}
		label := item.Status + " · " + item.Content
		rows = append(rows, []map[string]string{{"text": label, "callback_data": fmt.Sprintf("todo:%d:%s", i, item.Status)}})
	}
	if len(rows) == 0 {
		return simpleMenu("Todos", "No todos in this session")
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func compactionMenu() map[string]any {
	state := runtimeValues()
	auto := state.AutoCompaction
	if !state.AutoSet {
		auto = true
	}
	autoLabel := "⚙️ Auto: on"
	autoData := "compaction:auto_off"
	if !auto {
		autoLabel = "⚙️ Auto: off"
		autoData = "compaction:auto_on"
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🗜️ Compact now", "callback_data": "compact:now"}},
		{{"text": autoLabel, "callback_data": autoData}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}}
}

func permissionMenu() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🔒 Risky tool approvals", "callback_data": "permission:policy"}},
		{{"text": "✅ Always allow read/search", "callback_data": "permission:allow_read"}},
		{{"text": "↩️ Reset policy", "callback_data": "permission:reset"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}}
}

func snapshotMenu() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "↩️ Revert latest file", "callback_data": "revert:prompt"}},
		{{"text": "📋 List snapshots", "callback_data": "snapshots:list"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}}
}

func searchMenu() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🔎 Search index", "callback_data": "search:prompt"}},
		{{"text": "🗃️ Rebuild index", "callback_data": "index:rebuild"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}}
}

func indexMenu() map[string]any {
	return searchMenu()
}

func historyMenu() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "Last 5", "callback_data": "show_messages:5"}},
		{{"text": "Last 20", "callback_data": "show_messages:20"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}}
}

func commandMenu() map[string]any {
	rows := [][]map[string]string{}
	for _, command := range Commands() {
		name := "/" + strings.ReplaceAll(command.Command, "-", "_")
		rows = append(rows, []map[string]string{{"text": name, "callback_data": "run_command:" + name}})
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func worktreeMenu() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🌿 Git worktrees", "callback_data": "worktree:list"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}}
}

func simpleMenu(name, msg string) map[string]any {
	_ = name
	_ = msg
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "✕ Close", "callback_data": "close"}},
	}}
}

func Hub() map[string]any {
	labels := map[string]string{
		"status": "📊 Status", "session": "💬 Sessions", "model": "🧠 Model", "agent": "🤖 Agent",
		"variant": "💭 Variant", "project": "📁 Project", "tools": "🔧 Tools", "todos": "✅ Todos",
		"context": "🧮 Context", "compaction_level": "🗜️ Compact", "file_browser": "📂 Files",
		"settings": "⚙️ Settings", "permission": "🔒 Permissions", "search": "🔎 Search", "index": "🗃️ Index",
		"message_history": "🕘 History", "snapshots": "↩️ Snapshots", "skills_catalog": "🧩 Tools", "command_catalog": "⌘ Commands",
		"mcp_catalog": "🔌 MCP", "worktree": "🌿 Worktree",
	}
	rows := [][]map[string]string{}
	for i, name := range All {
		if i%2 == 0 {
			rows = append(rows, []map[string]string{{"text": labels[name], "callback_data": "menu:" + name}})
		} else {
			rows[len(rows)-1] = append(rows[len(rows)-1], map[string]string{"text": labels[name], "callback_data": "menu:" + name})
		}
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}
