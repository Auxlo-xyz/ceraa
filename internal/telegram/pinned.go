package telegram

import (
	"fmt"
	"strings"
	"sync"

	"ceraa/internal/logger"
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
	state     string
	reasoning string
	content   string
	updates   chan string
	closeOnce sync.Once
	done      chan struct{}
}

var pinnedByChat sync.Map

func NewPinnedManager(token string, chatID int64) *PinnedManager {
	manager := &PinnedManager{token: token, chatID: chatID, updates: make(chan string, 1), done: make(chan struct{})}
	go manager.runUpdates()
	return manager
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

func (p *PinnedManager) runUpdates() {
	defer close(p.done)
	for text := range p.updates {
		p.mu.Lock()
		messageID := p.messageID
		p.mu.Unlock()
		if messageID == 0 {
			continue
		}
		if err := postTelegramEditMessage(p.token, map[string]any{"chat_id": p.chatID, "message_id": messageID, "text": text}); err != nil {
			logger.Warn("Telegram pinned update failed: " + err.Error())
		}
	}
}

func (p *PinnedManager) close() {
	p.closeOnce.Do(func() {
		close(p.updates)
	})
	<-p.done
}

func (p *PinnedManager) Init() {
	p.mu.Lock()
	if p.messageID != 0 {
		p.mu.Unlock()
		return
	}
	text := p.formatLocked()
	p.mu.Unlock()
	messageID, err := sendMessagePayload(p.token, p.chatID, text, "", nil)
	if err != nil {
		logger.Warn("Telegram pinned initialization failed: " + err.Error())
		return
	}
	p.mu.Lock()
	p.messageID = messageID
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

func (p *PinnedManager) BeginRun() {
	p.update(func() {
		p.state = "Working"
		p.context = ""
		p.reasoning = ""
		p.content = ""
	})
}

func (p *PinnedManager) UpdateReasoning(reasoning string) {
	p.update(func() {
		p.reasoning = reasoning
	})
}

func (p *PinnedManager) UpdateContent(content string) {
	p.update(func() {
		p.content = content
	})
}

func (p *PinnedManager) ClearContent() {
	p.update(func() {
		p.content = ""
	})
}

func (p *PinnedManager) Complete() {
	p.update(func() {
		p.state = "Completed"
		p.context = "Run completed"
		p.reasoning = ""
		p.content = ""
	})
}

func (p *PinnedManager) Fail() {
	p.update(func() {
		p.state = "Failed"
		p.context = "Run failed"
		p.reasoning = ""
		p.content = ""
	})
}

func (p *PinnedManager) Stop() {
	p.update(func() {
		p.state = "Stopped"
		p.context = "Generation stopped"
		p.reasoning = ""
		p.content = ""
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
	if p.messageID == 0 {
		p.mu.Unlock()
		return
	}
	text := p.formatLocked()
	updates := p.updates
	p.mu.Unlock()
	if updates == nil {
		return
	}
	select {
	case updates <- text:
	default:
		select {
		case <-updates:
		default:
		}
		select {
		case updates <- text:
		default:
		}
	}
}

func (p *PinnedManager) formatLocked() string {
	state := p.state
	if state == "" {
		state = "Idle"
	}
	sections := []string{"📌 " + state, fmt.Sprintf("Tokens: %d", p.tokens), "Cost: " + p.cost}
	if p.context != "" {
		sections = append(sections, p.context)
	}
	if reasoning := strings.TrimSpace(p.reasoning); reasoning != "" {
		sections = append(sections, "🧠 "+truncateStatus(reasoning, 700))
	}
	if content := strings.TrimSpace(p.content); content != "" {
		sections = append(sections, "💬 "+truncateStatus(content, 1200))
	}
	if len(p.files) > 0 {
		sections = append(sections, "Files: "+fmt.Sprint(p.files))
	}
	return strings.Join(sections, "\n")
}

func formatToolStatus(status string) string {
	switch {
	case strings.HasPrefix(status, "🔄 "):
		return strings.TrimPrefix(status, "🔄 ")
	case strings.HasPrefix(status, "✅ "):
		return "✓ " + strings.TrimPrefix(status, "✅ ")
	case strings.HasPrefix(status, "⚠️ "):
		return "⚠ " + strings.TrimPrefix(status, "⚠️ ")
	default:
		return "• " + status
	}
}

func truncateStatus(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-3]) + "..."
}
