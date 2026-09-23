package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// PinnedManager maps to soraacode bot/pinned/pinned-message-manager.js
// Shows live tokens/cost/context/files, updated via editMessageText

type PinnedManager struct {
	mu        sync.Mutex
	chatID    int64
	messageID int
	token     string
	tokens    int
	cost      string
	context   string
	files     []string
}

func NewPinnedManager(token string, chatID int64) *PinnedManager {
	return &PinnedManager{token: token, chatID: chatID}
}

func (p *PinnedManager) Init() {
	p.mu.Lock()
	defer p.mu.Unlock()
	text := p.format()
	b, _ := json.Marshal(map[string]any{"chat_id": p.chatID, "text": text, "parse_mode": "Markdown"})
	resp, _ := http.Post(api("sendMessage", p.token), "application/json", bytes.NewReader(b))
	if resp != nil {
		defer resp.Body.Close()
		var out struct {
			Result struct {
				MessageID int `json:"message_id"`
			} `json:"result"`
		}
		// Simplified: not reading body for now, just store
		_ = out
		// In real, parse message_id from response
	}
}

func (p *PinnedManager) UpdateTokens(tokens int) {
	p.mu.Lock()
	p.tokens = tokens
	text := p.format()
	msgID := p.messageID
	p.mu.Unlock()
	if msgID != 0 {
		b, _ := json.Marshal(map[string]any{"chat_id": p.chatID, "message_id": msgID, "text": text, "parse_mode": "Markdown"})
		http.Post(api("editMessageText", p.token), "application/json", bytes.NewReader(b))
	}
}

func (p *PinnedManager) UpdateCost(cost string) {
	p.mu.Lock()
	p.cost = cost
	text := p.format()
	msgID := p.messageID
	p.mu.Unlock()
	if msgID != 0 {
		b, _ := json.Marshal(map[string]any{"chat_id": p.chatID, "message_id": msgID, "text": text, "parse_mode": "Markdown"})
		http.Post(api("editMessageText", p.token), "application/json", bytes.NewReader(b))
	}
}

func (p *PinnedManager) format() string {
	return fmt.Sprintf("📌 Pinned: tokens %d cost %s\n%s", p.tokens, p.cost, p.context)
}
