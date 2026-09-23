package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sooraa/internal/config"
	"sooraa/internal/telegram/menus"
	"strconv"
	"strings"

	"sooraa/harness/sessionstore"
)

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
		MessageID int `json:"message_id"`
	} `json:"message"`
}) {
	http.Post(api("answerCallbackQuery", token), "application/json", bytes.NewReader([]byte(`{"callback_query_id":"`+q.ID+`"}`)))
	if q.Message == nil {
		return
	}
	data := q.Data
	if data == "close" {
		b, _ := json.Marshal(map[string]any{"chat_id": q.Message.Chat.ID, "message_id": q.Message.MessageID, "reply_markup": map[string]any{"inline_keyboard": [][]any{}}})
		http.Post(api("editMessageReplyMarkup", token), "application/json", bytes.NewReader(b))
		return
	}
	if strings.HasPrefix(data, "menu:") {
		name := strings.TrimPrefix(data, "menu:")
		b, _ := json.Marshal(map[string]any{"chat_id": q.Message.Chat.ID, "message_id": q.Message.MessageID, "text": "📋 " + name, "reply_markup": menus.Inline(name, 0)})
		http.Post(api("editMessageText", token), "application/json", bytes.NewReader(b))
		return
	}
	// Harness-mapped selections -> map to soraacode app/managers/* and app/services/*
	if strings.HasPrefix(data, "select_model:") {
		model := strings.TrimPrefix(data, "select_model:")
		// Map to config model selection -> soraacode model-selection-service.js reconcileStoredModelSelection
		send(token, q.Message.Chat.ID, "Model selected: "+model+" (set via SOORAA_MODEL env or config.json)")
		return
	}
	if strings.HasPrefix(data, "select_agent:") {
		agent := strings.TrimPrefix(data, "select_agent:")
		send(token, q.Message.Chat.ID, "Agent: "+agent+" (harness agent switch stub - see internal/agent)")
		return
	}
	if strings.HasPrefix(data, "select_variant:") {
		variant := strings.TrimPrefix(data, "select_variant:")
		send(token, q.Message.Chat.ID, "Variant: "+variant)
		return
	}
	if strings.HasPrefix(data, "open_session:") {
		id := strings.TrimPrefix(data, "open_session:")
		store, _ := sessionstore.New("sessions")
		if store != nil {
			if sess, err := store.Load(id); err == nil {
				send(token, q.Message.Chat.ID, fmt.Sprintf("Session %s: %d messages", sess.ID, len(sess.Messages)))
				return
			}
		}
		send(token, q.Message.Chat.ID, "Session not found: "+id)
		return
	}
	if strings.HasPrefix(data, "browse:") {
		path := strings.TrimPrefix(data, "browse:")
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			b, _ := json.Marshal(map[string]any{"chat_id": q.Message.Chat.ID, "message_id": q.Message.MessageID, "text": "📁 " + path, "reply_markup": menus.Inline("file_browser", 0)})
			// Rebuild with actual path
			entries, _ := os.ReadDir(path)
			var rows [][]map[string]string
			for i, e := range entries {
				if i >= 8 {
					break
				}
				name := e.Name()
				if e.IsDir() {
					name += "/"
				}
				rows = append(rows, []map[string]string{{"text": name, "callback_data": "browse:" + filepath.Join(path, e.Name())}})
			}
			rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
			b, _ = json.Marshal(map[string]any{"chat_id": q.Message.Chat.ID, "message_id": q.Message.MessageID, "text": "📁 " + path, "reply_markup": map[string]any{"inline_keyboard": rows}})
			http.Post(api("editMessageText", token), "application/json", bytes.NewReader(b))
			return
		}
		// File -> show content via harness read
		data, err := os.ReadFile(path)
		if err != nil {
			send(token, q.Message.Chat.ID, "Read failed: "+err.Error())
			return
		}
		content := string(data)
		if len(content) > 3500 {
			content = content[:3500] + "..."
		}
		send(token, q.Message.Chat.ID, "```\n"+content+"\n```")
		return
	}
	if strings.HasPrefix(data, "show_messages:") {
		nStr := strings.TrimPrefix(data, "show_messages:")
		n, _ := strconv.Atoi(nStr)
		store, _ := sessionstore.New("sessions")
		if store != nil {
			if sess, err := store.Load(fmt.Sprint(q.Message.Chat.ID)); err == nil {
				start := len(sess.Messages) - n
				if start < 0 {
					start = 0
				}
				var lines []string
				for _, m := range sess.Messages[start:] {
					preview := m.Content
					if len(preview) > 80 {
						preview = preview[:80] + "..."
					}
					lines = append(lines, string(m.Role)+": "+preview)
				}
				send(token, q.Message.Chat.ID, strings.Join(lines, "\n"))
				return
			}
		}
		send(token, q.Message.Chat.ID, "No messages")
		return
	}
	if data == "worktree:list" {
		send(token, q.Message.Chat.ID, "Worktree: `git worktree list` - harness adapter stub")
		return
	}
	if strings.HasPrefix(data, "permission:") {
		perm := strings.TrimPrefix(data, "permission:")
		send(token, q.Message.Chat.ID, "Permission: "+perm+" (soraacode permission-manager generation)")
		return
	}
	if strings.HasPrefix(data, "context:") {
		action := strings.TrimPrefix(data, "context:")
		if action == "clear" {
			_ = clearSession(fmt.Sprint(q.Message.Chat.ID))
			send(token, q.Message.Chat.ID, "Context cleared")
		} else {
			cfg, _ := config.Load()
			send(token, q.Message.Chat.ID, fmt.Sprintf("Context: model %s, maxSteps %d", cfg.LLM.Model, cfg.MaxSteps))
		}
		return
	}
	if strings.HasPrefix(data, "compaction:") {
		level := strings.TrimPrefix(data, "compaction:")
		send(token, q.Message.Chat.ID, "Compaction: "+level+" (harness/contextbuilder stub)")
		return
	}
	if strings.HasPrefix(data, "task:") {
		send(token, q.Message.Chat.ID, "Scheduled tasks not yet wired - see harness/scheduler stub")
		return
	}
	// pagination or selection - no placeholder handling
	if strings.HasPrefix(data, "codex:check:") {
		flowID := strings.TrimPrefix(data, "codex:check:")
		state, err := authMgr.Check(flowID)
		if err != nil {
			send(token, q.Message.Chat.ID, "Login check failed: "+err.Error())
			return
		}
		b, _ := json.Marshal(map[string]any{
			"chat_id":      q.Message.Chat.ID,
			"text":         state.ResultMessage(),
			"reply_markup": json.RawMessage(`{"inline_keyboard":[]}`),
		})
		http.Post(api("editMessageText", token), "application/json", bytes.NewReader(b))
		return
	}
	if strings.HasPrefix(data, "codex:cancel:") {
		flowID := strings.TrimPrefix(data, "codex:cancel:")
		authMgr.Cancel(flowID)
		b, _ := json.Marshal(map[string]any{
			"chat_id":      q.Message.Chat.ID,
			"text":         "Login cancelled.",
			"reply_markup": json.RawMessage(`{"inline_keyboard":[]}`),
		})
		http.Post(api("editMessageText", token), "application/json", bytes.NewReader(b))
		return
	}
}
