package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"sooraa/harness/operation"
	"sooraa/harness/redact"
)

type runRenderer struct {
	token  string
	chatID int64
	pinned *PinnedManager

	mu            sync.Mutex
	messageID     int
	streamContent string
	lastStream    time.Time
	lastStatus    time.Time
	stopped       bool
	cancel        context.CancelFunc
	streamSignal  chan struct{}
	streamDone    chan struct{}
	streamClose   sync.Once
}

func newRunRenderer(token string, chatID int64, pinned *PinnedManager) *runRenderer {
	renderer := &runRenderer{
		token:        token,
		chatID:       chatID,
		pinned:       pinned,
		streamSignal: make(chan struct{}, 1),
		streamDone:   make(chan struct{}),
	}
	renderer.messageID = sendWithKeyboard(token, chatID, "💬", nil)
	pinned.BeginRun()
	go renderer.runStream()
	return renderer
}

func (r *runRenderer) runStream() {
	defer close(r.streamDone)
	for range r.streamSignal {
		r.mu.Lock()
		messageID := r.messageID
		content := r.streamContent
		r.mu.Unlock()
		if messageID == 0 {
			continue
		}
		_ = editTelegramMessage(r.token, r.chatID, messageID, truncateStream(content+" ▌"))
	}
}

func (r *runRenderer) signalStream() {
	select {
	case r.streamSignal <- struct{}{}:
	default:
	}
}

func (r *runRenderer) setCancel(cancel context.CancelFunc) {
	r.mu.Lock()
	r.cancel = cancel
	stopped := r.stopped
	r.mu.Unlock()
	if stopped && cancel != nil {
		cancel()
	}
}

func (r *runRenderer) close() {
	if r.streamSignal == nil {
		return
	}
	r.streamClose.Do(func() {
		close(r.streamSignal)
	})
	<-r.streamDone
}

func (r *runRenderer) reasoning(value string) {
	now := time.Now()
	r.mu.Lock()
	if now.Sub(r.lastStatus) < 300*time.Millisecond {
		r.mu.Unlock()
		return
	}
	r.lastStatus = now
	r.mu.Unlock()
	r.pinned.UpdateReasoning(value)
}

func (r *runRenderer) tool(value string) {
	if strings.HasPrefix(value, "🔄") || strings.HasPrefix(value, "✅") || strings.HasPrefix(value, "⚠️") {
		r.mu.Lock()
		r.streamContent = ""
		messageID := r.messageID
		r.mu.Unlock()
		if messageID == 0 {
			r.pinned.ClearContent()
		} else {
			r.signalStream()
		}
	}
	r.pinned.UpdateTool(value)
}

func (r *runRenderer) content(delta string) {
	r.mu.Lock()
	r.streamContent += delta
	content := r.streamContent
	messageID := r.messageID
	if time.Since(r.lastStream) < 350*time.Millisecond {
		r.mu.Unlock()
		return
	}
	r.lastStream = time.Now()
	r.mu.Unlock()
	if messageID == 0 {
		r.pinned.UpdateContent(content)
		return
	}
	r.signalStream()
}

func (r *runRenderer) finish(answer string) {
	r.mu.Lock()
	stopped := r.stopped
	messageID := r.messageID
	r.mu.Unlock()
	if stopped {
		return
	}
	if answer != "" {
		if messageID == 0 {
			sendWithKeyboard(r.token, r.chatID, answer, nil)
		} else if len([]rune(answer)) <= 4000 {
			_ = editTelegramMessageMode(r.token, r.chatID, messageID, answer, "Markdown")
		} else {
			runes := []rune(answer)
			_ = editTelegramMessageMode(r.token, r.chatID, messageID, string(runes[:4000]), "Markdown")
			sendWithKeyboard(r.token, r.chatID, string(runes[4000:]), nil)
		}
	}
	r.pinned.Complete()
}

func (r *runRenderer) fail() {
	r.mu.Lock()
	stopped := r.stopped
	r.mu.Unlock()
	if !stopped {
		r.pinned.Fail()
	}
}

func (r *runRenderer) stop() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	cancel := r.cancel
	r.mu.Unlock()
	r.pinned.Stop()
	if cancel != nil {
		cancel()
	}
}

func (r *runRenderer) isStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

func editTelegramMessage(token string, chatID int64, messageID int, text string) error {
	return editTelegramMessageMode(token, chatID, messageID, text, "")
}

func editTelegramMessageMode(token string, chatID int64, messageID int, text, parseMode string) error {
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	body, _ := json.Marshal(payload)
	resp, err := doTelegramPost(api("editMessageText", token), body)
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("editMessageText: empty response")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("editMessageText: HTTP %d: %s", resp.StatusCode, raw)
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("editMessageText response: %w", err)
	}
	if !result.OK && !strings.Contains(strings.ToLower(result.Description), "message is not modified") {
		return fmt.Errorf("editMessageText: %s", result.Description)
	}
	return nil
}

func truncateStream(value string) string {
	runes := []rune(value)
	if len(runes) <= 4000 {
		return value
	}
	return string(runes[len(runes)-3997:]) + "..."
}

func toolArgsSummary(name string, args map[string]any) string {
	raw, _ := args["args"].(string)
	if raw == "" {
		return ""
	}
	values := make(map[string]any)
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return ""
	}
	key := "value"
	switch name {
	case "read", "write", "edit", "move", "copy":
		key = "path"
	case "bash":
		key = "command"
	case "glob":
		key = "pattern"
	case "grep":
		key = "pattern"
	case "fts_search":
		key = "query"
	}
	value, ok := values[key]
	if !ok {
		return ""
	}
	text := strings.Join(strings.Fields(redact.SecretsInText(fmt.Sprint(value))), " ")
	if text == "" {
		return ""
	}
	return " · " + truncateStatus(text, 96)
}

func toolDisplayName(name string) string {
	switch name {
	case "read":
		return "Read file"
	case "write":
		return "Write file"
	case "edit":
		return "Edit file"
	case "bash":
		return "Run command"
	case "glob":
		return "Find files"
	case "grep":
		return "Search files"
	case "fts_search":
		return "Search project"
	default:
		if name == "" {
			return "Run tool"
		}
		return strings.ReplaceAll(name, "_", " ")
	}
}

func toolResultLabel(result operation.ResultEvent) string {
	switch result.Status {
	case operation.StatusCompleted:
		return "completed"
	case operation.StatusCanceled:
		return "cancelled"
	default:
		return "failed"
	}
}
