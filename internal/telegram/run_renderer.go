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
)

var activeDrafts sync.Map

type runRenderer struct {
	token   string
	chatID  int64
	pinned  *PinnedManager
	draftID int64

	mu           sync.Mutex
	draftEnabled bool
	draftContent string
	lastDraft    time.Time
	lastStatus   time.Time
	stopped      bool
	cancel       context.CancelFunc
	draftSignal  chan struct{}
	draftDone    chan struct{}
	draftClose   sync.Once
}

func newRunRenderer(token string, chatID int64, pinned *PinnedManager) *runRenderer {
	draftID := time.Now().UnixNano()
	if draftID == 0 {
		draftID = 1
	}
	renderer := &runRenderer{token: token, chatID: chatID, pinned: pinned, draftID: draftID, draftSignal: make(chan struct{}, 1), draftDone: make(chan struct{})}
	if err := sendMessageDraft(token, chatID, draftID, "", true); err == nil {
		renderer.draftEnabled = true
		activeDrafts.Store(draftID, renderer)
	}
	pinned.BeginRun()
	go renderer.runDrafts()
	return renderer
}

func (r *runRenderer) runDrafts() {
	defer close(r.draftDone)
	for range r.draftSignal {
		r.mu.Lock()
		enabled := r.draftEnabled
		content := r.draftContent
		r.mu.Unlock()
		if enabled {
			_ = sendMessageDraft(r.token, r.chatID, r.draftID, truncateDraft(content), true)
		}
	}
}

func (r *runRenderer) signalDraft() {
	select {
	case r.draftSignal <- struct{}{}:
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
	activeDrafts.Delete(r.draftID)
	if r.draftSignal == nil {
		return
	}
	r.draftClose.Do(func() {
		close(r.draftSignal)
	})
	<-r.draftDone
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
		r.draftContent = ""
		enabled := r.draftEnabled
		r.mu.Unlock()
		if enabled {
			r.signalDraft()
		} else {
			r.pinned.ClearContent()
		}
	}
	r.pinned.UpdateTool(value)
}

func (r *runRenderer) content(delta string) {
	r.mu.Lock()
	r.draftContent += delta
	content := r.draftContent
	enabled := r.draftEnabled
	if time.Since(r.lastDraft) < 350*time.Millisecond {
		r.mu.Unlock()
		return
	}
	r.lastDraft = time.Now()
	r.mu.Unlock()
	if !enabled {
		r.pinned.UpdateContent(content)
		return
	}
	r.signalDraft()
}

func (r *runRenderer) finish(answer string) {
	r.mu.Lock()
	stopped := r.stopped
	r.mu.Unlock()
	if stopped {
		return
	}
	if answer != "" {
		send(r.token, r.chatID, answer)
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

func sendMessageDraft(token string, chatID, draftID int64, text string, canStop bool) error {
	body, _ := json.Marshal(map[string]any{
		"chat_id":      chatID,
		"draft_id":     draftID,
		"text":         text,
		"can_stop":     canStop,
		"keep_on_stop": false,
	})
	resp, err := doTelegramPost(api("sendMessageDraft", token), body)
	if err != nil {
		return fmt.Errorf("sendMessageDraft: %w", err)
	}
	if resp == nil {
		return fmt.Errorf("sendMessageDraft: empty response")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sendMessageDraft: HTTP %d: %s", resp.StatusCode, raw)
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("sendMessageDraft response: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("sendMessageDraft: %s", result.Description)
	}
	return nil
}

func truncateDraft(value string) string {
	runes := []rune(value)
	if len(runes) <= 4096 {
		return value
	}
	return "…" + string(runes[len(runes)-4095:])
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
