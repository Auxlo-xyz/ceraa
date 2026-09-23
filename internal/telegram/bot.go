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

	"github.com/emperormk01/soraa-agent/internal/agent"
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

func send(token string, chatID int64, text string) {
	if len(text) > 4000 {
		text = text[:4000]
	}
	b, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": text, "parse_mode": "Markdown"})
	http.Post(api("sendMessage", token), "application/json", bytes.NewReader(b))
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
			go func(prompt string) {
				send(token, chatID, "_Working..._")
				// capture agent output by redirecting? agent.Run prints; run and send result
				// simple: call agent and on finish send done; agent.Run handles LLM+tools
				// For now run via channel: agent returns via stdout, we send completion note
				// Re-run with direct capture: use custom runner that returns text
				done := make(chan string, 1)
				go func() {
					// hijack stdout capture not needed - just run and notify
					err := agent.Run(prompt)
					if err != nil {
						done <- "error: " + err.Error()
					} else {
						done <- "Done."
					}
				}()
				select {
				case msg := <-done:
					send(token, chatID, msg)
				case <-time.After(10 * time.Minute):
					send(token, chatID, "Timeout.")
				}
			}(txt)
		}
	}
}
