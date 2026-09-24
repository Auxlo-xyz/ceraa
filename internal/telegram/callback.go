package telegram

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"sooraa/harness/memory"
	"sooraa/harness/permission"
	"sooraa/harness/question"
	"sooraa/harness/scheduler"
	"sooraa/harness/skills"
	"sooraa/harness/todo"
	"sooraa/harness/tool"
	"sooraa/internal/config"
	"sooraa/internal/telegram/menus"
)

func answerCallback(token, id string) {
	if id == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"callback_query_id": id})
	go func() {
		resp, _ := doTelegramPost(api("answerCallbackQuery", token), body)
		discardTelegramResponse(resp)
	}()
}

func editCallback(token string, chatID int64, messageID int, text string, markup map[string]any) {
	payload := map[string]any{"chat_id": chatID, "message_id": messageID}
	if text != "" {
		payload["text"] = text
	}
	if markup == nil {
		payload["reply_markup"] = map[string]any{"inline_keyboard": [][]any{}}
	} else {
		payload["reply_markup"] = markup
	}
	body, _ := json.Marshal(payload)
	resp, _ := doTelegramPost(api("editMessageText", token), body)
	discardTelegramResponse(resp)
}

func removeCallbackMarkup(token string, chatID int64, messageID int, text string) {
	emptyMarkup := map[string]any{"inline_keyboard": [][]any{}}
	body, _ := json.Marshal(map[string]any{"chat_id": chatID, "message_id": messageID, "reply_markup": emptyMarkup})
	resp, err := doTelegramPost(api("editMessageReplyMarkup", token), body)
	if resp != nil {
		var result struct {
			Ok bool `json:"ok"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err == nil && decodeErr == nil && result.Ok {
			return
		}
	}
	if text == "" {
		text = "Closed"
	}
	body, _ = json.Marshal(map[string]any{"chat_id": chatID, "message_id": messageID, "text": text, "reply_markup": emptyMarkup})
	resp, _ = doTelegramPost(api("editMessageText", token), body)
	discardTelegramResponse(resp)
}

func handleCallback(token string, q *struct {
	ID   string `json:"id"`
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Data    string `json:"data"`
	Message *struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		MessageID int    `json:"message_id"`
		Text      string `json:"text"`
	} `json:"message"`
}) {
	answerCallback(token, q.ID)
	if q.Message == nil {
		fmt.Printf("callback missing message id=%s data=%q\n", q.ID, q.Data)
		return
	}
	fmt.Printf("callback handle id=%s data=%q message=%d\n", q.ID, q.Data, q.Message.MessageID)
	chatID := q.Message.Chat.ID
	data := q.Data
	if data == "close" || data == "close_menu" {
		removeCallbackMarkup(token, chatID, q.Message.MessageID, q.Message.Text)
		return
	}
	if data == "noop" {
		return
	}
	if strings.HasPrefix(data, "onboarding:provider:") {
		providerID := strings.TrimPrefix(data, "onboarding:provider:")
		draft := providerDraft{ID: providerID, Stage: "model"}
		switch providerID {
		case "openrouter":
			draft.URL = "https://openrouter.ai/api/v1"
		case "poolside":
			draft.URL = "https://inference.poolside.ai/v1"
		case "custom":
			draft.Stage = "url"
		default:
			send(token, chatID, "Unknown provider choice")
			return
		}
		pendingProvider.Store(chatID, draft)
		if draft.Stage == "url" {
			send(token, chatID, "Send the custom provider base URL.")
		} else {
			send(token, chatID, "Send the model ID for "+providerID+".")
		}
		return
	}
	if data == "onboarding:claim" {
		if err := defaultState.setAllowedUserID(q.From.ID); err != nil {
			send(token, chatID, "Could not save Telegram allowlist: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "🔒 Bot locked to your Telegram account", nil)
		return
	}
	if data == "memory:view" {
		editCallback(token, chatID, q.Message.MessageID, "🧠 Compact memory", menus.InlineForChat("memory", 0, chatID, currentSessionID(chatID)))
		return
	}
	if data == "memory:add" {
		pendingMemory.Store(chatID, true)
		send(token, chatID, "Send the compact fact or preference to remember. I’ll save it as the next message.")
		return
	}
	if strings.HasPrefix(data, "memory:remove:") {
		cfg, err := config.Load()
		if err == nil {
			store, storeErr := memory.New(cfg.MemoryFile)
			if storeErr == nil {
				if err := store.Remove(strings.TrimPrefix(data, "memory:remove:")); err != nil {
					send(token, chatID, "Memory removal failed: "+err.Error())
					return
				}
			}
		}
		editCallback(token, chatID, q.Message.MessageID, "Memory removed", menus.InlineForChat("memory", 0, chatID, currentSessionID(chatID)))
		return
	}
	if data == "skill:view" {
		editCallback(token, chatID, q.Message.MessageID, "🧩 Skill proposals", menus.InlineForChat("skill", 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "skill:approve:") || strings.HasPrefix(data, "skill:reject:") {
		cfg, err := config.Load()
		if err != nil {
			send(token, chatID, "Config error: "+err.Error())
			return
		}
		store, err := skills.New(cfg.SkillFile)
		if err != nil {
			send(token, chatID, "Skill store error: "+err.Error())
			return
		}
		action := "approve"
		id := strings.TrimPrefix(data, "skill:approve:")
		if strings.HasPrefix(data, "skill:reject:") {
			action = "reject"
			id = strings.TrimPrefix(data, "skill:reject:")
		}
		result := "approved"
		if action == "approve" {
			_, err = store.Approve(id)
		} else {
			result = "rejected"
			err = store.Reject(id)
		}
		if err != nil {
			send(token, chatID, "Skill update failed: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "Skill proposal "+result, menus.InlineForChat("skill", 0, chatID, currentSessionID(chatID)))
		return
	}
	if data == "eval:view" {
		handleEvalCommand(token, chatID)
		return
	}
	if data == "schedule:view" {
		editCallback(token, chatID, q.Message.MessageID, "⏰ Scheduled jobs", menus.InlineForChat("schedule", 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "schedule:preset:") {
		interval, err := time.ParseDuration(strings.TrimPrefix(data, "schedule:preset:"))
		if err != nil || interval < time.Minute {
			send(token, chatID, "Invalid schedule interval")
			return
		}
		pendingSchedule.Store(chatID, interval)
		send(token, chatID, "Send the task to run on that schedule as the next message.")
		return
	}
	if strings.HasPrefix(data, "schedule:remove:") {
		cfg, err := config.Load()
		if err == nil {
			store, storeErr := scheduler.New(cfg.SchedulerFile)
			if storeErr == nil {
				_, _ = store.Remove(strings.TrimPrefix(data, "schedule:remove:"))
			}
		}
		editCallback(token, chatID, q.Message.MessageID, "Scheduled job removed", menus.InlineForChat("schedule", 0, chatID, currentSessionID(chatID)))
		return
	}
	if data == "setting:steps" {
		handleStepsCommand(token, chatID, "/steps")
		return
	}
	if strings.HasPrefix(data, "steps:") {
		handleStepsCommand(token, chatID, "/steps "+strings.TrimPrefix(data, "steps:"))
		return
	}
	if strings.HasPrefix(data, "menu:") {
		name := strings.TrimPrefix(data, "menu:")
		editCallback(token, chatID, q.Message.MessageID, "📋 "+name, menus.InlineForChat(name, 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "select_model:") {
		model := strings.TrimPrefix(data, "select_model:")
		if err := defaultState.setModel(model); err != nil {
			send(token, chatID, "Failed to set model: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "✅ Model set to "+model, nil)
		return
	}
	if strings.HasPrefix(data, "select_provider:") {
		provider := strings.TrimPrefix(data, "select_provider:")
		if err := defaultState.setProvider(provider); err != nil {
			send(token, chatID, "Provider save failed: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "✅ Provider set to "+provider, menus.InlineForChat("model", 0, chatID, currentSessionID(chatID)))
		return
	}
	if data == "provider:custom" {
		pendingProvider.Store(chatID, providerDraft{})
		send(token, chatID, "Send the provider base URL for the custom profile.")
		return
	}
	if strings.HasPrefix(data, "select_agent:") {
		agent := strings.TrimPrefix(data, "select_agent:")
		if err := defaultState.setAgent(agent); err != nil {
			send(token, chatID, "Failed to set agent: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "✅ Agent set to "+agent, nil)
		return
	}
	if strings.HasPrefix(data, "select_variant:") {
		variant := strings.TrimPrefix(data, "select_variant:")
		if err := defaultState.setVariant(variant); err != nil {
			send(token, chatID, "Failed to set variant: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "✅ Variant set to "+variant, nil)
		return
	}
	if strings.HasPrefix(data, "attach_session:") || strings.HasPrefix(data, "open_session:") {
		id := strings.TrimPrefix(data, "attach_session:")
		if strings.HasPrefix(data, "open_session:") {
			id = strings.TrimPrefix(data, "open_session:")
		}
		store, err := configuredStore()
		if err != nil {
			send(token, chatID, "Session store error: "+err.Error())
			return
		}
		sess, err := store.Load(id)
		if err != nil || sess.ID != id || id == "inbox" {
			send(token, chatID, "Session not found: "+id)
			return
		}
		if err := defaultState.setSession(chatID, id); err != nil {
			send(token, chatID, "Failed to save active session: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, fmt.Sprintf("✅ Attached to `%s` (%d messages)", id, len(sess.Messages)), menus.InlineForChat("session", 0, chatID, id))
		return
	}
	if data == "new_session" {
		startNewSession(token, chatID)
		return
	}
	if strings.HasPrefix(data, "project:") {
		path := strings.TrimPrefix(data, "project:")
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			send(token, chatID, "Project directory not found: "+path)
			return
		}
		if err := defaultState.setProject(chatID, path); err != nil {
			send(token, chatID, "Failed to set project: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "📁 Project set to "+defaultState.project(chatID), nil)
		return
	}
	if strings.HasPrefix(data, "browse:") {
		handleBrowseCallback(token, chatID, q.Message.MessageID, strings.TrimPrefix(data, "browse:"))
		return
	}
	if strings.HasPrefix(data, "show_messages:") {
		n, _ := strconv.Atoi(strings.TrimPrefix(data, "show_messages:"))
		store, err := configuredStore()
		if err != nil {
			send(token, chatID, "Session store error: "+err.Error())
			return
		}
		sess, err := store.Load(currentSessionID(chatID))
		if err != nil {
			send(token, chatID, "No messages")
			return
		}
		start := len(sess.Messages) - n
		if start < 0 {
			start = 0
		}
		var lines []string
		for _, message := range sess.Messages[start:] {
			preview := message.Content
			if preview == "" {
				preview = message.ToolResult
			}
			if len(preview) > 120 {
				preview = preview[:120] + "..."
			}
			lines = append(lines, string(message.Role)+": "+preview)
		}
		send(token, chatID, strings.Join(lines, "\n"))
		return
	}
	if strings.HasPrefix(data, "tool:") {
		name := strings.TrimPrefix(data, "tool:")
		if registered, ok := tool.DefaultRegistry().Get(name); ok {
			send(token, chatID, fmt.Sprintf("🔧 `%s`\n%s\nSchema: `%s`", name, registered.Description(), mustJSON(registered.Schema())))
			return
		}
		send(token, chatID, "Tool not found: "+name)
		return
	}
	if strings.HasPrefix(data, "todo:") {
		handleTodoCallback(token, chatID, q.Message.MessageID, strings.TrimPrefix(data, "todo:"))
		return
	}
	if strings.HasPrefix(data, "permission:") {
		handlePermissionMenuCallback(token, chatID, strings.TrimPrefix(data, "permission:"))
		return
	}
	if strings.HasPrefix(data, "perm:") {
		parts := strings.Split(data, ":")
		if len(parts) < 3 {
			send(token, chatID, "Invalid permission response")
			return
		}
		decision := parts[1]
		requestID := parts[2]
		toolName := ""
		if len(parts) > 3 {
			toolName = parts[3]
		}
		if permission.Default.Answer(currentSessionID(chatID), requestID, decision) {
			if decision == "allowAlways" && toolName != "" {
				permission.Default.AllowAlways(toolName)
				_ = defaultState.setAlwaysAllow(toolName, true)
			}
			editCallback(token, chatID, q.Message.MessageID, "Permission "+decision+" recorded", nil)
		} else {
			send(token, chatID, "Permission request expired or was already answered")
		}
		return
	}
	if strings.HasPrefix(data, "qans:") {
		parts := strings.SplitN(data, ":", 3)
		if len(parts) < 3 {
			send(token, chatID, "Invalid answer")
			return
		}
		if question.Default.Answer(currentSessionID(chatID), parts[1], parts[2]) {
			editCallback(token, chatID, q.Message.MessageID, "✓ Answered: "+parts[2], nil)
		} else {
			send(token, chatID, "Question already answered or expired")
		}
		return
	}
	if strings.HasPrefix(data, "context:") {
		action := strings.TrimPrefix(data, "context:")
		if action == "clear" {
			store, err := configuredStore()
			if err == nil {
				err = store.Clear(currentSessionID(chatID))
			}
			if err != nil {
				send(token, chatID, "Context clear failed: "+err.Error())
				return
			}
			send(token, chatID, "Context cleared for the active session")
		} else {
			handleContextCommand(token, chatID)
		}
		return
	}
	if data == "compaction:auto_on" {
		handleCompaction(token, chatID, "/compaction auto on")
		return
	}
	if data == "compaction:auto_off" {
		handleCompaction(token, chatID, "/compaction auto off")
		return
	}
	if strings.HasPrefix(data, "compact:") || strings.HasPrefix(data, "compaction:") {
		handleCompaction(token, chatID, "/compaction now")
		return
	}
	if data == "search:prompt" {
		send(token, chatID, "Send /search <query> to search the active project index.")
		return
	}
	if data == "index:rebuild" {
		handleIndexCommand(token, chatID, "/index")
		return
	}
	if data == "snapshots:list" {
		handleSnapshotsCommand(token, chatID)
		return
	}
	if data == "revert:prompt" {
		send(token, chatID, "Send /revert <path> to restore the latest snapshot for that file.")
		return
	}
	if strings.HasPrefix(data, "run_command:") {
		executeCallbackCommand(token, chatID, strings.TrimPrefix(data, "run_command:"))
		return
	}
	if data == "worktree:list" {
		output, err := exec.Command("git", "worktree", "list").CombinedOutput()
		if err != nil {
			send(token, chatID, "Worktree lookup failed: "+err.Error())
			return
		}
		send(token, chatID, string(output))
		return
	}
	if strings.HasPrefix(data, "codex:check:") {
		flowID := strings.TrimPrefix(data, "codex:check:")
		state, err := authMgr.Check(flowID)
		if err != nil {
			send(token, chatID, "Login check failed: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, state.ResultMessage(), map[string]any{"inline_keyboard": [][]any{}})
		return
	}
	if strings.HasPrefix(data, "codex:cancel:") {
		authMgr.Cancel(strings.TrimPrefix(data, "codex:cancel:"))
		editCallback(token, chatID, q.Message.MessageID, "Login cancelled.", map[string]any{"inline_keyboard": [][]any{}})
		return
	}
}

func handleBrowseCallback(token string, chatID int64, messageID int, path string) {
	root := defaultState.project(chatID)
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		send(token, chatID, "Invalid path")
		return
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		send(token, chatID, "Path is outside the active project")
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		send(token, chatID, "Path not found: "+rel)
		return
	}
	if info.IsDir() {
		editCallback(token, chatID, messageID, "📁 "+rel, menus.InlinePath(abs))
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		send(token, chatID, "Read failed: "+err.Error())
		return
	}
	content := string(data)
	if len(content) > 3500 {
		content = content[:3500] + "...[truncated]"
	}
	send(token, chatID, "```\n"+content+"\n```")
}

func handleTodoCallback(token string, chatID int64, messageID int, value string) {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return
	}
	index, err := strconv.Atoi(parts[0])
	if err != nil {
		return
	}
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
	if err != nil || index < 0 || index >= len(items) {
		send(token, chatID, "Todo item not found")
		return
	}
	if items[index].Status == "completed" {
		items[index].Status = "pending"
	} else {
		items[index].Status = "completed"
	}
	if err := store.Save(currentSessionID(chatID), items); err != nil {
		send(token, chatID, "Todo update failed: "+err.Error())
		return
	}
	editCallback(token, chatID, messageID, "✅ Todo updated", menus.InlineForChat("todos", 0, chatID, currentSessionID(chatID)))
}

func handlePermissionMenuCallback(token string, chatID int64, action string) {
	switch action {
	case "policy":
		send(token, chatID, "🔒 Write, delete, move, copy, and risky shell operations require approval. Read/search tools run without approval.")
	case "allow_read":
		for _, name := range []string{"read", "glob", "grep", "fts_search"} {
			permission.Default.AllowAlways(name)
			_ = defaultState.setAlwaysAllow(name, true)
		}
		send(token, chatID, "✅ Read/search tools will be allowed without prompts.")
	case "reset":
		for _, name := range []string{"read", "glob", "grep", "fts_search", "write", "bash"} {
			permission.Default.ClearAlways(name)
			_ = defaultState.setAlwaysAllow(name, false)
		}
		send(token, chatID, "Permission policy reset.")
	default:
		send(token, chatID, "Unknown permission action")
	}
}

func executeCallbackCommand(token string, chatID int64, command string) {
	command = strings.TrimSpace(command)
	switch command {
	case "/status":
		handleStatus(token, chatID)
	case "/settings":
		handleSettings(token, chatID)
	case "/sessions":
		handleSessions(token, chatID)
	case "/messages":
		handleMessages(token, chatID)
	case "/tools":
		handleToolsCommand(token, chatID)
	case "/todos":
		handleTodosCommand(token, chatID)
	case "/context":
		handleContextCommand(token, chatID)
	case "/model":
		handleModelCommand(token, chatID, command)
	case "/agent":
		handleAgentCommand(token, chatID, command)
	case "/variant":
		handleVariantCommand(token, chatID, command)
	case "/project":
		handleProjectCommand(token, chatID, command)
	case "/permission":
		handlePermissionCommand(token, chatID)
	case "/compaction", "/compaction_level":
		handleCompaction(token, chatID, command)
	case "/index":
		handleIndexCommand(token, chatID, command)
	case "/snapshots":
		handleSnapshotsCommand(token, chatID)
	case "/revert":
		handleRevertCommand(token, chatID, command)
	case "/file_browser":
		sendMenuMarkup(token, chatID, "📂 Workspace files", menus.InlineForChat("file_browser", 0, chatID, currentSessionID(chatID)))
	case "/skills_catalog":
		handleCatalogCommand(token, chatID, command)
	case "/command_catalog":
		handleCatalogCommand(token, chatID, command)
	case "/mcp_catalog":
		handleCatalogCommand(token, chatID, command)
	case "/steps":
		handleStepsCommand(token, chatID, command)
	case "/help":
		handleFullHelp(token, chatID)
	case "/new":
		startNewSession(token, chatID)
	case "/detach":
		detachedChats.Store(chatID, true)
		send(token, chatID, "🔌 Detached. Send /attach to reattach.")
	case "/attach":
		detachedChats.Delete(chatID)
		send(token, chatID, "🔗 Attached to session "+currentSessionID(chatID))
	case "/codex-login", "/codex_login":
		handleCodexLogin(token, chatID)
	case "/codex-status", "/codex_status":
		handleCodexStatus(token, chatID)
	case "/menu":
		sendMenuMarkup(token, chatID, "📋 Sooraa control hub", menus.Hub())
	case "/search":
		handleSearchCommand(token, chatID, command)
	default:
		send(token, chatID, "Use that command with an argument, for example /"+strings.TrimPrefix(command, "/")+" value")
	}
}

func mustJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
