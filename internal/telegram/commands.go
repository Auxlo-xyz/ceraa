package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"sooraa/harness/search"
	"sooraa/harness/sessionstore"
	"sooraa/harness/snapshot"
	"sooraa/harness/todo"
	"sooraa/harness/tool"
	"sooraa/internal/config"
	"sooraa/internal/telegram/menus"
)

func commandCatalog() []menus.CommandSpec {
	return menus.Commands()
}

func commandNames() []map[string]string {
	catalog := commandCatalog()
	out := make([]map[string]string, 0, len(catalog))
	for _, command := range catalog {
		name := strings.ReplaceAll(command.Command, "-", "_")
		out = append(out, map[string]string{"command": name, "description": command.Description})
	}
	return out
}
func configuredStore() (*sessionstore.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return sessionstore.New(cfg.SessionFile)
}

func currentSessionID(chatID int64) string {
	return defaultState.sessionID(chatID)
}

func startNewSession(token string, chatID int64) {
	store, err := configuredStore()
	if err != nil {
		send(token, chatID, "Failed to create session: "+err.Error())
		return
	}
	id := fmt.Sprintf("tg-%d", time.Now().UnixNano())
	if err := store.Save(&sessionstore.Session{ID: id, Version: 2}); err != nil {
		send(token, chatID, "Failed to create session: "+err.Error())
		return
	}
	if err := defaultState.setSession(chatID, id); err != nil {
		send(token, chatID, "Session created, but state save failed: "+err.Error())
		return
	}
	send(token, chatID, "🆕 New session started: "+id)
}

func sendMenuMarkup(token string, chatID int64, text string, markup map[string]any) {
	body, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": text, "reply_markup": markup})
	resp, _ := doTelegramPost(api("sendMessage", token), body)
	discardTelegramResponse(resp)
}

func handleModelCommand(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		sendMenuMarkup(token, chatID, "Select a model", menusInline("model"))
		return
	}
	if err := defaultState.setModel(parts[1]); err != nil {
		send(token, chatID, "Failed to set model: "+err.Error())
		return
	}
	send(token, chatID, "✅ Model set to "+parts[1])
}

func handleAgentCommand(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		sendMenuMarkup(token, chatID, "Select an agent", menusInline("agent"))
		return
	}
	agent := parts[1]
	if agent != "build" && agent != "plan" {
		send(token, chatID, "Agent must be build or plan")
		return
	}
	if err := defaultState.setAgent(agent); err != nil {
		send(token, chatID, "Failed to set agent: "+err.Error())
		return
	}
	send(token, chatID, "✅ Agent set to "+agent)
}

func handleVariantCommand(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		sendMenuMarkup(token, chatID, "Select a response variant", menusInline("variant"))
		return
	}
	variant := parts[1]
	if variant != "default" && variant != "compact" {
		send(token, chatID, "Variant must be default or compact")
		return
	}
	if err := defaultState.setVariant(variant); err != nil {
		send(token, chatID, "Failed to set variant: "+err.Error())
		return
	}
	send(token, chatID, "✅ Variant set to "+variant)
}

func handleStepsCommand(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		sendMenuMarkup(token, chatID, "Select max steps", map[string]any{"inline_keyboard": [][]map[string]string{
			{{"text": "5", "callback_data": "steps:5"}, {"text": "10", "callback_data": "steps:10"}},
			{{"text": "20", "callback_data": "steps:20"}, {"text": "30", "callback_data": "steps:30"}},
			{{"text": "✕ Close", "callback_data": "close"}},
		}})
		return
	}
	steps, err := strconv.Atoi(parts[1])
	if err != nil {
		send(token, chatID, "Steps must be a number from 1 to 50")
		return
	}
	if err := defaultState.setMaxSteps(steps); err != nil {
		send(token, chatID, "Invalid steps: use a value from 1 to 50")
		return
	}
	send(token, chatID, fmt.Sprintf("✅ Max steps set to %d", steps))
}

func handleProjectCommand(token string, chatID int64, text string) {
	path := strings.TrimSpace(strings.TrimPrefix(text, "/project"))
	if path == "" {
		sendMenuMarkup(token, chatID, "Select a project", menus.InlineForChat("project", 0, chatID, currentSessionID(chatID)))
		return
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		send(token, chatID, "Project must be an existing directory")
		return
	}
	if err := defaultState.setProject(chatID, path); err != nil {
		send(token, chatID, "Failed to set project: "+err.Error())
		return
	}
	send(token, chatID, "✅ Project set to "+defaultState.project(chatID))
}

func handleToolsCommand(token string, chatID int64) {
	var lines []string
	for _, registered := range tool.DefaultRegistry().All() {
		description := registered.Description()
		if len(description) > 140 {
			description = description[:140] + "..."
		}
		lines = append(lines, fmt.Sprintf("🔧 `%s` — %s", registered.Name(), description))
	}
	if len(lines) == 0 {
		send(token, chatID, "No tools registered")
		return
	}
	text := "Available tools:\n" + strings.Join(lines, "\n")
	sendMenuMarkup(token, chatID, text, menus.Inline("tools", 0))
}

func handleTodosCommand(token string, chatID int64) {
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	store, err := todo.New(cfg.SessionFile)
	if err != nil {
		send(token, chatID, "Todo store error: "+err.Error())
		return
	}
	items, err := store.Load(currentSessionID(chatID))
	if err != nil {
		send(token, chatID, "No todos available")
		return
	}
	if len(items) == 0 {
		send(token, chatID, "No todos in this session")
		return
	}
	var lines []string
	for i, item := range items {
		lines = append(lines, fmt.Sprintf("%d. [%s] %s", i+1, item.Status, item.Content))
	}
	text := "Todos:\n" + strings.Join(lines, "\n")
	sendMenuMarkup(token, chatID, text, menus.InlineForChat("todos", 0, chatID, currentSessionID(chatID)))
}

func handleContextCommand(token string, chatID int64) {
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	store, err := configuredStore()
	if err != nil {
		send(token, chatID, "Session store error: "+err.Error())
		return
	}
	tokens, messages := 0, 0
	if sess, err := store.Load(currentSessionID(chatID)); err == nil {
		messages = len(sess.Messages)
		for _, message := range sess.Messages {
			tokens += (len(message.Content) + len(message.ToolResult) + len(message.Reasoning)) / 4
		}
	}
	percent := 0
	if cfg.LLM.ContextLimit > 0 {
		percent = tokens * 100 / cfg.LLM.ContextLimit
	}
	text := fmt.Sprintf("📊 Context\n~%d / %d tokens (%d%%)\nSession: `%s`\nProject: `%s`\nMessages: %d\nModel: %s", tokens, cfg.LLM.ContextLimit, percent, currentSessionID(chatID), defaultState.project(chatID), messages, cfg.LLM.Model)
	sendMenuMarkup(token, chatID, text, menus.InlineForSession("status", 0, currentSessionID(chatID)))
}

func handleSearchCommand(token string, chatID int64, text string) {
	query := strings.TrimSpace(strings.TrimPrefix(text, "/search"))
	if query == "" {
		sendMenuMarkup(token, chatID, "Search workspace", menusInline("search"))
		return
	}
	idx, err := search.Open(search.DefaultPath())
	if err != nil {
		send(token, chatID, "Search index error: "+err.Error())
		return
	}
	defer idx.Close()
	if _, err := idx.IndexDir(defaultState.project(chatID)); err != nil {
		send(token, chatID, "Index error: "+err.Error())
		return
	}
	results, err := idx.Search(query, 10)
	if err != nil {
		send(token, chatID, "Search error: "+err.Error())
		return
	}
	if len(results) == 0 {
		send(token, chatID, "No matches for "+query)
		return
	}
	var lines []string
	for i, result := range results {
		lines = append(lines, fmt.Sprintf("%d. `%s` (rank %.2f)", i+1, result.Path, result.Rank))
	}
	send(token, chatID, "Search results:\n"+strings.Join(lines, "\n"))
}

func handleIndexCommand(token string, chatID int64, text string) {
	path := strings.TrimSpace(strings.TrimPrefix(text, "/index"))
	if path == "" {
		path = defaultState.project(chatID)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		send(token, chatID, "Index path must be an existing directory")
		return
	}
	idx, err := search.Open(search.DefaultPath())
	if err != nil {
		send(token, chatID, "Index error: "+err.Error())
		return
	}
	defer idx.Close()
	count, err := idx.IndexDir(path)
	if err != nil {
		send(token, chatID, "Index error: "+err.Error())
		return
	}
	send(token, chatID, fmt.Sprintf("🗃️ Indexed %d files from %s", count, path))
}

func handleCatalogCommand(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return
	}
	switch strings.TrimPrefix(parts[0], "/") {
	case "skills_catalog":
		handleToolsCommand(token, chatID)
	case "command_catalog":
		handleFullHelp(token, chatID)
	case "mcp_catalog":
		send(token, chatID, "🔌 MCP catalog: no MCP servers configured")
	default:
		send(token, chatID, "Unknown catalog")
	}
}

func handleSnapshotsCommand(token string, chatID int64) {
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	store, err := snapshot.New(filepath.Join(cfg.SessionFile, ".snapshots"))
	if err != nil {
		send(token, chatID, "Snapshot store error: "+err.Error())
		return
	}
	entries, err := store.Entries()
	if err != nil {
		send(token, chatID, "Snapshot error: "+err.Error())
		return
	}
	if len(entries) == 0 {
		send(token, chatID, "No snapshots yet. File writes create snapshots automatically.")
		return
	}
	var lines []string
	for i, entry := range entries {
		if i >= 10 {
			break
		}
		state := "existing"
		if !entry.Exists {
			state = "new file"
		}
		lines = append(lines, fmt.Sprintf("%d. `%s` (%s, %s)", i+1, entry.Path, state, entry.Timestamp.Format("2006-01-02 15:04")))
	}
	send(token, chatID, "Snapshots:\n"+strings.Join(lines, "\n")+"\n\nUse /revert <path> to restore the latest snapshot.")
}

func handleRevertCommand(token string, chatID int64, text string) {
	path := strings.TrimSpace(strings.TrimPrefix(text, "/revert"))
	if path == "" {
		send(token, chatID, "Usage: /revert <path>")
		return
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(defaultState.project(chatID), path)
	}
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	store, err := snapshot.New(filepath.Join(cfg.SessionFile, ".snapshots"))
	if err != nil {
		send(token, chatID, "Snapshot store error: "+err.Error())
		return
	}
	entry, err := store.RevertLatest(path)
	if err != nil {
		send(token, chatID, "Revert failed: "+err.Error())
		return
	}
	send(token, chatID, "↩️ Restored "+entry.Path)
}

func handlePermissionCommand(token string, chatID int64) {
	sendMenuMarkup(token, chatID, "Permission controls", menusInline("permission"))
}

func menusInline(name string) map[string]any {
	return menusInlineForSession(name, "")
}

func menusInlineForSession(name, sessionID string) map[string]any {
	return menus.InlineForSession(name, 0, sessionID)
}

func handleSessions(token string, chatID int64) {
	store, err := configuredStore()
	if err != nil {
		send(token, chatID, "Failed to open sessions: "+err.Error())
		return
	}
	ids, err := store.List()
	if err != nil {
		send(token, chatID, "Failed to list sessions: "+err.Error())
		return
	}
	if len(ids) == 0 {
		send(token, chatID, "No sessions yet. Use /new to start.")
		return
	}
	var lines []string
	for i, id := range ids {
		sess, err := store.Load(id)
		if err != nil || sess.ID != id || id == "inbox" {
			continue
		}
		if i >= 10 {
			lines = append(lines, fmt.Sprintf("... and %d more", len(ids)-10))
			break
		}
		lines = append(lines, fmt.Sprintf("• `%s` (%d messages)", id, len(sess.Messages)))
	}
	text := "Sessions:\n" + strings.Join(lines, "\n")
	sendMenuMarkup(token, chatID, text, menus.InlineForChat("session", 0, chatID, currentSessionID(chatID)))
}

// handleOpenSession loads a session by ID -> maps to soraacode session.ts get
func handleOpenSession(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		send(token, chatID, "Usage: /open <session_id>\nList with /sessions")
		return
	}
	id := parts[1]
	store, err := configuredStore()
	if err != nil {
		send(token, chatID, "Failed: "+err.Error())
		return
	}
	sess, err := store.Load(id)
	if err != nil {
		send(token, chatID, "Session not found: "+id)
		return
	}
	if err := defaultState.setSession(chatID, sess.ID); err != nil {
		send(token, chatID, "Session opened, but state save failed: "+err.Error())
		return
	}
	send(token, chatID, fmt.Sprintf("Opened session `%s` (%d messages)", sess.ID, len(sess.Messages)))
}

// handleMessages shows recent messages -> maps to soraacode session/history.ts
func handleMessages(token string, chatID int64) {
	store, err := configuredStore()
	if err != nil {
		send(token, chatID, "Failed: "+err.Error())
		return
	}
	sess, err := store.Load(currentSessionID(chatID))
	if err != nil {
		send(token, chatID, "No session yet.")
		return
	}
	if len(sess.Messages) == 0 {
		send(token, chatID, "No messages in current session.")
		return
	}
	start := len(sess.Messages) - 5
	if start < 0 {
		start = 0
	}
	var lines []string
	for _, m := range sess.Messages[start:] {
		role := string(m.Role)
		if m.ToolCall != nil {
			role = "tool:" + m.ToolCall.Name
		} else if m.ToolCallID != "" {
			role = "tool_result"
		}
		preview := m.Content
		if len(preview) > 120 {
			preview = preview[:120] + "..."
		}
		if preview == "" && m.ToolResult != "" {
			preview = m.ToolResult
			if len(preview) > 120 {
				preview = preview[:120] + "..."
			}
		}
		lines = append(lines, fmt.Sprintf("**%s**: %s", role, preview))
	}
	send(token, chatID, "Recent messages:\n"+strings.Join(lines, "\n"))
}

// handleListFiles glob/list directory -> maps to soraacode filesystem.ts + glob.ts
func handleListFiles(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	path := defaultState.project(chatID)
	if len(parts) >= 2 {
		path = parts[1]
		if !filepath.IsAbs(path) {
			path = filepath.Join(defaultState.project(chatID), path)
		}
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		data, err := os.ReadFile(path)
		if err != nil {
			send(token, chatID, "Read failed: "+err.Error())
			return
		}
		content := string(data)
		if len(content) > 3500 {
			content = content[:3500] + "...[truncated]"
		}
		send(token, chatID, "```\n"+content+"\n```")
		return
	}
	// List directory
	entries, err := os.ReadDir(path)
	if err != nil {
		send(token, chatID, "Failed: "+err.Error())
		return
	}
	if len(entries) == 0 {
		send(token, chatID, "Empty: "+path)
		return
	}
	var lines []string
	for i, e := range entries {
		if i >= 30 {
			lines = append(lines, fmt.Sprintf("... and %d more", len(entries)-30))
			break
		}
		suffix := ""
		if e.IsDir() {
			suffix = "/"
		}
		lines = append(lines, e.Name()+suffix)
	}
	send(token, chatID, "Files in `"+path+"`:\n"+strings.Join(lines, "\n"))
}

// handleStatus shows harness status -> maps to soraacode session/status.ts + llm/route
func handleStatus(token string, chatID int64) {
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	store, _ := configuredStore()
	var sessCount int
	var msgCount int
	if store != nil {
		if ids, err := store.List(); err == nil {
			sessCount = len(ids)
		}
		if sess, err := store.Load(currentSessionID(chatID)); err == nil {
			msgCount = len(sess.Messages)
		}
	}
	model, agent, variant, steps := defaultState.values()
	if model == "" {
		model = cfg.LLM.Model
	}
	if agent == "" {
		agent = "build"
	}
	if variant == "" {
		variant = "default"
	}
	if steps == 0 {
		steps = cfg.MaxSteps
	}
	text := fmt.Sprintf("Sooraa status:\n• Session: `%s`\n• Project: `%s`\n• Model: %s\n• Agent: %s\n• Variant: %s\n• URL: %s\n• Sessions: %d\n• Current messages: %d\n• Max steps: %d",
		currentSessionID(chatID), defaultState.project(chatID), model, agent, variant, cfg.LLM.URL, sessCount, msgCount, steps)
	sendMenuMarkup(token, chatID, text, menus.InlineForSession("status", 0, currentSessionID(chatID)))
}

// handleSettings shows config -> maps to soraacode config.ts
func handleSettings(token string, chatID int64) {
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	providers := make([]string, 0, len(cfg.Subscriptions.Providers))
	for name, provider := range cfg.Subscriptions.Providers {
		kind := provider.Kind
		if kind == "" {
			kind = "configured"
		}
		providers = append(providers, name+":"+kind)
	}
	text := fmt.Sprintf("⚙️ Safe settings\n• Model: %s\n• Endpoint: %s\n• Context limit: %d\n• Max output: %d\n• Max steps: %d\n• Session store: %s\n• Telegram user: configured\n• Subscriptions: %s", cfg.LLM.Model, cfg.LLM.URL, cfg.LLM.ContextLimit, cfg.LLM.MaxOutputTokens, cfg.MaxSteps, cfg.SessionFile, strings.Join(providers, ", "))
	b, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": text, "reply_markup": map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🧠 Model", "callback_data": "menu:model"}, {"text": "🔁 Steps", "callback_data": "setting:steps"}},
		{{"text": "🧮 Context", "callback_data": "menu:context"}, {"text": "✕ Close", "callback_data": "close"}},
	}}})
	resp, _ := doTelegramPost(api("sendMessage", token), b)
	discardTelegramResponse(resp)
}

// handleDeleteSession deletes a session -> maps to soraacode session.ts delete
func handleDeleteSession(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		send(token, chatID, "Usage: /delete <session_id>")
		return
	}
	id := parts[1]
	store, err := configuredStore()
	if err != nil {
		send(token, chatID, "Failed: "+err.Error())
		return
	}
	// Clear session
	sess, err := store.Load(id)
	if err != nil {
		send(token, chatID, "Session not found: "+id)
		return
	}
	if err := sess.Clear(); err != nil {
		send(token, chatID, "Delete failed: "+err.Error())
		return
	}
	cfg, err := config.Load()
	if err == nil {
		_ = os.Remove(filepath.Join(cfg.SessionFile, id+".json"))
	}
	send(token, chatID, "Deleted session "+id)
}

// handleRenameSession renames session file -> maps to soraacode rename-manager.js
func handleRenameSession(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 3 {
		send(token, chatID, "Usage: /rename <old_id> <new_id>")
		return
	}
	oldID := parts[1]
	newID := parts[2]
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	oldPath := filepath.Join(cfg.SessionFile, oldID+".json")
	newPath := filepath.Join(cfg.SessionFile, newID+".json")
	if _, err := os.Stat(oldPath); err != nil {
		send(token, chatID, "Session not found: "+oldID)
		return
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		send(token, chatID, "Rename failed: "+err.Error())
		return
	}
	if currentSessionID(chatID) == oldID {
		_ = defaultState.setSession(chatID, newID)
	}
	send(token, chatID, fmt.Sprintf("Renamed %s -> %s", oldID, newID))
}

// handleAbort cancels running operations -> maps to soraacode session/interrupt.ts + run-control
func handleAbort(token string, chatID int64) {
	if cancel, ok := activeRuns.Load(chatID); ok {
		if cancelFunc, ok := cancel.(context.CancelFunc); ok {
			cancelFunc()
			send(token, chatID, "🛑 Current run cancelled")
			return
		}
	}
	send(token, chatID, "No active run to cancel")
}

// handleCompaction shows compaction level -> maps to soraacode session/compaction.ts + overflow.ts + context-epoch.ts
func handleCompaction(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) == 1 {
		cfg, _ := config.Load()
		store, _ := configuredStore()
		tokens, compactions := 0, 0
		if store != nil {
			if sess, err := store.Load(currentSessionID(chatID)); err == nil {
				for _, message := range sess.Messages {
					tokens += (len(message.Content) + len(message.ToolResult) + len(message.Reasoning)) / 4
					if message.Compaction != nil {
						compactions++
					}
				}
			}
		}
		auto, configured := defaultState.autoCompaction()
		if !configured {
			auto = true
		}
		limit := 0
		if cfg != nil {
			limit = cfg.LLM.ContextLimit
		}
		text := fmt.Sprintf("🗜️ Compaction status\n• Tokens: ~%d / %d\n• Auto: %t\n• Compactions: %d\n• Keep: 8k recent tokens", tokens, limit, auto, compactions)
		sendMenuMarkup(token, chatID, text, menus.Inline("compaction_level", 0))
		return
	}
	level := parts[1]
	if level == "auto" && len(parts) >= 3 {
		enabled := parts[2] == "on" || parts[2] == "true"
		if parts[2] != "on" && parts[2] != "off" && parts[2] != "true" && parts[2] != "false" {
			send(token, chatID, "Usage: /compaction auto on|off")
			return
		}
		if err := defaultState.setAutoCompaction(enabled); err != nil {
			send(token, chatID, "Failed to save compaction setting: "+err.Error())
			return
		}
		send(token, chatID, fmt.Sprintf("✅ Automatic compaction %s", map[bool]string{true: "enabled", false: "disabled"}[enabled]))
		return
	}
	if level == "compact" || level == "now" {
		requestForcedCompaction(currentSessionID(chatID))
		send(token, chatID, "🗜️ Compaction queued for the next prompt in this session")
		return
	}
	send(token, chatID, "Usage: /compaction [now|auto on|auto off]")
}

// handleWorktree handles git worktree -> maps to soraacode worktree-service.js
func handleWorktree(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) == 1 || parts[1] == "list" {
		output, err := exec.Command("git", "worktree", "list").CombinedOutput()
		if err != nil {
			send(token, chatID, "Worktree lookup failed: "+err.Error())
			return
		}
		send(token, chatID, strings.TrimSpace(string(output)))
		return
	}
	send(token, chatID, "Usage: /worktree [list]")
}

// handleHelp shows full command list -> maps to soraacode bot/commands/definitions.js
func handleFullHelp(token string, chatID int64) {
	catalog := commandCatalog()
	lines := make([]string, 0, len(catalog)+2)
	lines = append(lines, "Sooraa commands:")
	for _, command := range catalog {
		lines = append(lines, "/"+command.Command+" - "+command.Description)
	}
	lines = append(lines, "", "Send any other text to run the agent.")
	send(token, chatID, strings.Join(lines, "\n"))
}
