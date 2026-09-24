package telegram

import (
	"encoding/json"
	"fmt"
	"sync"
)

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

var pinnedByChat sync.Map

func NewPinnedManager(token string, chatID int64) *PinnedManager {
	return &PinnedManager{token: token, chatID: chatID}
}

func getPinnedManager(token string, chatID int64) *PinnedManager {
	key := fmt.Sprintf("%d", chatID)
	if value, ok := pinnedByChat.Load(key); ok {
		return value.(*PinnedManager)
	}
	manager := NewPinnedManager(token, chatID)
	actual, _ := pinnedByChat.LoadOrStore(key, manager)
	return actual.(*PinnedManager)
}

func (p *PinnedManager) Init() {
	p.mu.Lock()
	if p.messageID != 0 {
		p.mu.Unlock()
		return
	}
	text := p.formatLocked()
	p.mu.Unlock()
	body, _ := json.Marshal(map[string]any{"chat_id": p.chatID, "text": text})
	resp, err := doTelegramPost(api("sendMessage", p.token), body)
	if err != nil || resp == nil {
		return
	}
	defer resp.Body.Close()
	var result struct {
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return
	}
	p.mu.Lock()
	p.messageID = result.Result.MessageID
	p.mu.Unlock()
}

func (p *PinnedManager) UpdateTokens(tokens int) {
	p.update(func() {
		p.tokens = tokens
	})
}

func (p *PinnedManager) UpdateCost(cost string) {
	p.update(func() {
		p.cost = cost
	})
}

func (p *PinnedManager) UpdateContext(context string) {
	p.update(func() {
		p.context = context
	})
}

func (p *PinnedManager) AddFile(path string) {
	p.update(func() {
		p.files = append(p.files, path)
		if len(p.files) > 5 {
			p.files = p.files[len(p.files)-5:]
		}
	})
}

func (p *PinnedManager) update(change func()) {
	p.mu.Lock()
	change()
	text := p.formatLocked()
	messageID := p.messageID
	p.mu.Unlock()
	if messageID == 0 {
		return
	}
	body, _ := json.Marshal(map[string]any{"chat_id": p.chatID, "message_id": messageID, "text": text})
	resp, _ := doTelegramPost(api("editMessageText", p.token), body)
	discardTelegramResponse(resp)
}

func (p *PinnedManager) formatLocked() string {
	files := ""
	if len(p.files) > 0 {
		files = "\nFiles: " + fmt.Sprint(p.files)
	}
	return fmt.Sprintf("📌 Live status\nTokens: %d\nCost: %s\n%s%s", p.tokens, p.cost, p.context, files)
}
