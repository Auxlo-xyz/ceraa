package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type update struct {
	UpdateID int `json:"update_id"`
	Message  *struct {
		Chat struct{ ID int64 `json:"id"` } `json:"chat"`
		From struct{ ID int64 `json:"id"` } `json:"from"`
		Text string `json:"text"`
	} `json:"message"`
}

func api(method, token string) string { return "https://api.telegram.org/bot" + token + "/" + method }

func send(token string, chatID int64, text string) int {
	if len(text) > 4000 {
		text = text[:4000]
	}
	b, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": text, "parse_mode": "Markdown"})
	resp, _ := http.Post(api("sendMessage", token), "application/json", bytes.NewReader(b))
	if resp == nil {
		return 0
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out struct {
		Result struct{ MessageID int `json:"message_id"` } `json:"result"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.Result.MessageID
}

func sendThinking(token string, chatID int64, reasoning string, collapsed bool) int {
	header := "💭 Thinking..."
	text := header + "\n" + strings.TrimSpace(reasoning)
	if len(text) > 4000 {
		text = text[:4000]
	}
	typ := "blockquote"
	if collapsed {
		typ = "expandable_blockquote"
	}
	entities := []map[string]any{{"type": typ, "offset": len(header) + 1, "length": len(text) - len(header) - 1}}
	b, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": text, "entities": entities})
	resp, _ := http.Post(api("sendMessage", token), "application/json", bytes.NewReader(b))
	if resp == nil {
		return 0
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out struct {
		Result struct{ MessageID int `json:"message_id"` } `json:"result"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.Result.MessageID
}

func editThinking(token string, chatID int64, msgID int, reasoning string, collapsed bool) {
	header := "💭 Thinking..."
	text := header + "\n" + strings.TrimSpace(reasoning)
	if strings.TrimSpace(reasoning) == "" {
		text = header
	}
	if len(text) > 4000 {
		text = text[:4000]
	}
	typ := "blockquote"
	if collapsed {
		typ = "expandable_blockquote"
	}
	entities := []map[string]any{{"type": typ, "offset": len(header) + 1, "length": len(text) - len(header) - 1}}
	b, _ := json.Marshal(map[string]any{"chat_id": chatID, "message_id": msgID, "text": text, "entities": entities})
	http.Post(api("editMessageText", token), "application/json", bytes.NewReader(b))
}

func Run() error {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN required")
	}
	allow := os.Getenv("TELEGRAM_ALLOWED_USER_ID")
	var allowID int64
	if allow != "" {
		allowID, _ = strconv.ParseInt(strings.TrimSpace(allow), 10, 64)
	}
	offset := 0
	fmt.Println("telegram polling start")
	for {
		resp, err := http.Get(api(fmt.Sprintf("getUpdates?timeout=30&offset=%d", offset), token))
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var out struct {
			Ok     bool     `json:"ok"`
			Result []update `json:"result"`
		}
		_ = json.Unmarshal(raw, &out)
		for _, u := range out.Result {
			offset = u.UpdateID + 1
			if u.Message == nil || u.Message.Text == "" {
				continue
			}
			if allowID != 0 && u.Message.From.ID != allowID && u.Message.Chat.ID != allowID {
				send(token, u.Message.Chat.ID, "Unauthorized.")
				continue
			}
			txt := strings.TrimSpace(u.Message.Text)
			if txt == "/start" || txt == "/help" {
				send(token, u.Message.Chat.ID, "Soraa Agent ready. Send a task.")
				continue
			}
			chatID := u.Message.Chat.ID
			go handlePrompt(token, chatID, txt)
		}
	}
}

func handlePrompt(token string, chatID int64, prompt string) {
	thinkingID := sendThinking(token, chatID, "", false)
	toolID := 0
	var toolLines []string
	var reasoning string
	lastEdit := time.Now()
	done := make(chan string, 1)
	go func() {
		err := runWithThinking(prompt, func(r string) {
			reasoning = r
			if time.Since(lastEdit) > 1200*time.Millisecond {
				editThinking(token, chatID, thinkingID, reasoning, false)
				lastEdit = time.Now()
			}
		}, func(t string) {
			toolLines = append(toolLines, t)
			text := strings.Join(toolLines, "\n")
			if toolID == 0 {
				toolID = send(token, chatID, "```\n"+text+"\n```")
			} else {
				editTool(token, chatID, toolID, text)
			}
		}, func(answer string) {
			done <- answer
		})
		if err != nil {
			done <- "error: " + err.Error()
		}
	}()
	select {
	case ans := <-done:
		editThinking(token, chatID, thinkingID, reasoning, true)
		if ans != "" {
			send(token, chatID, ans)
		}
	case <-time.After(10 * time.Minute):
		send(token, chatID, "Timeout.")
	}
}

func editTool(token string, chatID int64, msgID int, text string) {
	if len(text) > 3500 {
		text = text[len(text)-3500:]
	}
	b, _ := json.Marshal(map[string]any{"chat_id": chatID, "message_id": msgID, "text": "```\n" + text + "\n```", "parse_mode": "Markdown"})
	http.Post(api("editMessageText", token), "application/json", bytes.NewReader(b))
}

func runWithThinking(prompt string, onReasoning func(string), onTool func(string), onAnswer func(string)) error {
	return runAgent(prompt, onReasoning, onTool, onAnswer)
}
