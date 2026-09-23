package telegram

import (
	"bytes"
	"encoding/json"
	"net/http"
	"soraa-agent/internal/telegram/menus"
	"strings"
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
		// Remove inline keyboard and mark closed - Telegram requires editMessageReplyMarkup or delete
		b, _ := json.Marshal(map[string]any{"chat_id": q.Message.Chat.ID, "message_id": q.Message.MessageID, "reply_markup": map[string]any{"inline_keyboard": [][]any{}}})
		http.Post(api("editMessageReplyMarkup", token), "application/json", bytes.NewReader(b))
		return
	}
	if strings.HasPrefix(data, "menu:") {
		name := strings.TrimPrefix(data, "menu:")
		b, _ := json.Marshal(map[string]any{"chat_id": q.Message.Chat.ID, "message_id": q.Message.MessageID, "text": name + " - no items yet", "reply_markup": menus.Inline(name, 0)})
		http.Post(api("editMessageText", token), "application/json", bytes.NewReader(b))
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
