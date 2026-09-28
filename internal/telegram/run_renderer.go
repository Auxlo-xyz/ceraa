package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"strings"
	"sync"
	"time"

	"ceraa/harness/operation"
	"ceraa/harness/redact"
	"ceraa/internal/logger"
	"ceraa/internal/telegram/keyboard"
)

type toolActivity struct {
	id     string
	status string
	label  string
	detail string
}

type runRenderer struct {
	token  string
	chatID int64

	mu            sync.Mutex
	messageID     int
	thoughtID     int
	streamContent string
	thought       string
	tools         []toolActivity
	lastThought   time.Time
	lastStatus    time.Time
	// Last text accepted by the API for each message, used to skip no-op edits.
	//
	// The stream loop re-sends the whole accumulated buffer on every signal,
	// because Telegram has no append. Without this, any signal that did not change
	// the rendered text still PUT the entire buffer, and Telegram answered
	// "message is not modified". Live cost: 404 such rejections, and a run that
	// ended in 429 retry after 227s because a single 3k answer was re-sent around
	// thirty times.
	sentStream   string
	sentThought  string
	lastSignal   time.Time
	stopped      bool
	cancel       context.CancelFunc
	streamSignal chan struct{}
	streamDone   chan struct{}
	streamMu     sync.Mutex
	streamClosed bool
}

func newRunRenderer(token string, chatID int64, _ ...*PinnedManager) *runRenderer {
	renderer := &runRenderer{
		token:        token,
		chatID:       chatID,
		streamSignal: make(chan struct{}, 1),
		streamDone:   make(chan struct{}),
	}
	// The rich branch is the one production takes: the endpoint works, so the
	// plain fallback below is dead in practice. Attaching the keyboard only to the
	// fallback is why a prompt finished with no keyboard while /status, which goes
	// down the plain path, looked correct. Both branches now carry it.
	if messageID, err := sendRichMessage(token, chatID, "▌"); err == nil {
		renderer.messageID = messageID
		// The rich payload cannot carry the keyboard, so attach it separately.
		if err := attachReplyKeyboard(token, chatID, messageID); err != nil {
			logger.Warn("reply keyboard attach failed: " + err.Error())
		}
	} else {
		// Carry the reply keyboard from creation. Every later step of the run only
		// edits this message, and an edit that omits reply_markup leaves the
		// existing one in place, so this one call covers the streamed text, the
		// final answer, and the rich-message replacement.
		renderer.messageID = sendWithKeyboard(token, chatID, "💬", keyboard.Main())
	}
	go renderer.runStream()
	return renderer
}

func (r *runRenderer) runStream() {
	defer close(r.streamDone)
	// A final render once the channel is closed.
	//
	// Pacing means the last state change can have had its signal dropped: a tool
	// that completes inside the interval never gets a signal of its own, and the
	// queued one may already have been consumed against the earlier state. Without
	// this the live preview can show a tool as still running right up to the final
	// answer, which is the only place the completed state would otherwise appear.
	defer r.renderCurrentState()
	for range r.streamSignal {
		r.renderCurrentState()
	}
}

// renderCurrentState pushes the current stream and thought text, skipping either
// when the API already has it.
//
// Shared by the loop and the final render so the two cannot drift on what counts
// as changed.
func (r *runRenderer) renderCurrentState() {
	{
		r.mu.Lock()
		messageID := r.messageID
		text := r.streamTextLocked()
		thoughtID := r.thoughtID
		thoughtText := r.thoughtTextLocked()
		skipStream := text == r.sentStream
		skipThought := thoughtID != 0 && thoughtText == r.sentThought
		r.mu.Unlock()
		if thoughtID != 0 && thoughtText != "" && !skipThought {
			if err := editTelegramHTMLMessage(r.token, r.chatID, thoughtID, thoughtText); err != nil {
				if fallbackErr := editTelegramMessage(r.token, r.chatID, thoughtID, thoughtFallbackText(thoughtText)); fallbackErr != nil {
					logger.Warn("Telegram thought preview fallback failed: " + fallbackErr.Error())
				}
			}
			r.mu.Lock()
			r.sentThought = thoughtText
			r.mu.Unlock()
		}
		if messageID == 0 || skipStream {
			return
		}
		if err := editTelegramRichMessage(r.token, r.chatID, messageID, text); err != nil {
			if fallbackErr := editTelegramMessage(r.token, r.chatID, messageID, text); fallbackErr != nil {
				logger.Warn("Telegram streaming preview fallback failed: " + fallbackErr.Error())
				return
			}
		}
		// Recorded only after the API accepted the text. A rejected edit leaves the
		// last-known-good value in place, so the next signal retries it instead of
		// treating the message as already up to date.
		r.mu.Lock()
		r.sentStream = text
		r.mu.Unlock()
	}
}

// streamMinInterval is the floor between two outbound stream edits.
//
// The three producers used to pace themselves independently: content at 350ms,
// reasoning at 300ms, and tool with no limit at all. A burst of tool results
// therefore signalled on every single event, and each signal cost a full-buffer
// edit. Coalescing here, at the one point every producer funnels through, bounds
// the rate regardless of which producer is busy.
//
// Dropping a signal is safe: streamContent keeps accumulating, so the next one
// that is let through renders the newer text rather than losing it. The final
// answer does not go through here at all, so a dropped tail is always superseded
// by the real answer.
const streamMinInterval = 300 * time.Millisecond

func (r *runRenderer) signalStream() {
	r.streamMu.Lock()
	defer r.streamMu.Unlock()
	if r.streamClosed {
		return
	}
	if time.Since(r.lastSignal) < streamMinInterval {
		return
	}
	r.lastSignal = time.Now()
	select {
	case r.streamSignal <- struct{}{}:
	default:
	}
}

func (r *runRenderer) streamTextLocked() string {
	parts := make([]string, 0, 2)
	if len(r.tools) > 0 {
		parts = append(parts, formatToolActivity(r.tools))
	}
	if content := strings.TrimSpace(r.streamContent); content != "" {
		parts = append(parts, truncateStream(content+" ▌"))
	} else if len(parts) == 0 {
		parts = append(parts, "▌")
	}
	return strings.Join(parts, "\n\n")
}

func (r *runRenderer) thoughtTextLocked() string {
	if strings.TrimSpace(r.thought) == "" {
		return ""
	}
	return formatThought(r.thought, true)
}

func formatThought(value string, streaming bool) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if streaming {
		value += " ▌"
	}
	return "<blockquote expandable><b>Thought</b>\n\n" + html.EscapeString(truncateStream(value)) + "</blockquote>"
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
	r.streamMu.Lock()
	if !r.streamClosed {
		r.streamClosed = true
		close(r.streamSignal)
	}
	r.streamMu.Unlock()
	<-r.streamDone
}

func (r *runRenderer) reasoning(value string) {
	now := time.Now()
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.thought = value
	thoughtID := r.thoughtID
	oldMessageID := r.messageID
	assistantPreview := r.streamTextLocked()
	shouldStream := now.Sub(r.lastThought) >= 300*time.Millisecond
	if shouldStream {
		r.lastThought = now
	}
	r.lastStatus = now
	r.mu.Unlock()
	if !shouldStream {
		return
	}
	if thoughtID == 0 {
		thoughtMessageID, err := sendHTMLMessage(r.token, r.chatID, formatThought(value, true))
		if err != nil {
			thoughtMessageID = sendPlainMessage(r.token, r.chatID, thoughtFallbackText(formatThought(value, true)))
		}
		if thoughtMessageID == 0 {
			return
		}
		assistantMessageID, assistantErr := sendRichMessage(r.token, r.chatID, assistantPreview)
		if assistantErr != nil {
			assistantMessageID = sendWithKeyboard(r.token, r.chatID, assistantPreview, nil)
		}
		if assistantMessageID == 0 {
			return
		}
		if oldMessageID != 0 && oldMessageID != assistantMessageID {
			if deleteErr := deleteTelegramMessage(r.token, r.chatID, oldMessageID); deleteErr != nil {
				logger.Warn("Telegram assistant preview replacement cleanup failed: " + deleteErr.Error())
			}
		}
		r.mu.Lock()
		r.thoughtID = thoughtMessageID
		r.messageID = assistantMessageID
		r.mu.Unlock()
		// sentStream is deliberately not seeded here. It looks necessary, since the
		// replacement is created already carrying the preview, but a probe showed
		// sentStream already equals that preview by this point: a queued signal
		// renders it before reasoning() gets here. Seeding it was redundant, and
		// because it was redundant no test could tell it from its absence.
	}
	r.signalStream()
}

func (r *runRenderer) tool(callID, value string) {
	activity, ok := parseToolActivity(value)
	if !ok {
		return
	}
	activity.id = callID
	r.mu.Lock()
	r.streamContent = ""
	if activity.status == "🔄" || activity.status == "⚡" {
		r.tools = append(r.tools, activity)
	} else {
		index := -1
		if callID != "" {
			for i := len(r.tools) - 1; i >= 0; i-- {
				if r.tools[i].id == callID {
					index = i
					break
				}
			}
		}
		if index < 0 {
			for i := len(r.tools) - 1; i >= 0; i-- {
				if r.tools[i].label == activity.label {
					index = i
					break
				}
			}
		}
		if index >= 0 {
			r.tools[index].status = activity.status
			if activity.detail != "" {
				r.tools[index].detail = activity.detail
			}
		} else {
			r.tools = append(r.tools, activity)
		}
	}
	messageID := r.messageID
	r.mu.Unlock()
	if messageID != 0 {
		r.signalStream()
	}
}

func (r *runRenderer) content(delta string) {
	r.mu.Lock()
	r.streamContent += delta
	r.mu.Unlock()
	// Pacing lives in signalStream now, so a stream tick cannot outrun the others.
	r.signalStream()
}

func (r *runRenderer) finish(answer string) {
	r.mu.Lock()
	stopped := r.stopped
	r.mu.Unlock()
	if stopped {
		r.close()
		return
	}
	r.close()
	r.mu.Lock()
	stopped = r.stopped
	messageID := r.messageID
	thoughtID := r.thoughtID
	thought := r.thought
	r.mu.Unlock()
	if stopped {
		return
	}
	if thoughtID != 0 && strings.TrimSpace(thought) != "" {
		if !r.publishThoughtMessage(formatThought(thought, false), thoughtID) {
			logger.Warn("Telegram final thought publication failed")
		}
	}
	r.publishFinalAnswer(answer, messageID)
}

// publishFinalAnswer delivers the answer and makes sure the keyboard follows it.
//
// Extracted so the contract is testable: the answer is the message the user
// reads, and the one that has to end up carrying the keyboard. A green suite that
// only exercised message creation passed while this path silently stopped
// attaching anything.
func (r *runRenderer) publishFinalAnswer(answer string, messageID int) {
	if answer != "" {
		runes := []rune(answer)
		if len(runes) <= telegramRichMessageLimit {
			if !r.publishRichMessage(answer, messageID) {
				if messageID == 0 {
					sendPlainMessage(r.token, r.chatID, answer)
				} else {
					if err := editTelegramMessage(r.token, r.chatID, messageID, answer); err != nil {
						logger.Warn("Telegram final message fallback failed: " + err.Error())
						// The edit failed, so nothing is carrying the keyboard. Fall
						// back to a fresh send, which does.
						if sendPlainMessage(r.token, r.chatID, answer) == 0 {
							logger.Warn("Telegram final answer could not be delivered")
						}
					} else if err := attachReplyKeyboard(r.token, r.chatID, messageID); err != nil {
						// The answer landed but the keyboard did not. Reported rather
						// than retried: editMessageReplyMarkup succeeds about 85% of
						// the time, and hammering a rejection here turns one wasted
						// call into several.
						logger.Warn("reply keyboard attach failed: " + err.Error())
					}
				}
			}
		} else if messageID == 0 {
			for rest := answer; len([]rune(rest)) > 0; {
				chunkRunes := []rune(rest)
				size := telegramRichMessageLimit
				if len(chunkRunes) < size {
					size = len(chunkRunes)
				}
				chunk := string(chunkRunes[:size])
				rest = string(chunkRunes[size:])
				if _, err := sendRichMessage(r.token, r.chatID, chunk); err != nil {
					sendPlainMessage(r.token, r.chatID, chunk)
				}
			}
		} else {
			first := string(runes[:telegramRichMessageLimit])
			if !r.publishRichMessage(first, messageID) {
				if err := editTelegramMessage(r.token, r.chatID, messageID, first); err != nil {
					logger.Warn("Telegram long message fallback failed: " + err.Error())
				}
			}
			for rest := string(runes[telegramRichMessageLimit:]); len([]rune(rest)) > 0; {
				chunkRunes := []rune(rest)
				size := telegramRichMessageLimit
				if len(chunkRunes) < size {
					size = len(chunkRunes)
				}
				chunk := string(chunkRunes[:size])
				rest = string(chunkRunes[size:])
				if _, err := sendRichMessage(r.token, r.chatID, chunk); err != nil {
					sendPlainMessage(r.token, r.chatID, chunk)
				}
			}
		}
	}
}

func (r *runRenderer) fail() {
	r.close()
	r.mu.Lock()
	stopped := r.stopped
	thoughtID := r.thoughtID
	thought := r.thought
	r.mu.Unlock()
	if stopped {
		return
	}
	if thoughtID != 0 && strings.TrimSpace(thought) != "" {
		if !r.publishThoughtMessage(formatThought(thought, false), thoughtID) {
			logger.Warn("Telegram failed thought publication failed")
		}
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
	if cancel != nil {
		cancel()
	}
}

func (r *runRenderer) isStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

func (r *runRenderer) publishThoughtMessage(text string, messageID int) bool {
	if messageID != 0 {
		if err := editTelegramHTMLMessage(r.token, r.chatID, messageID, text); err == nil {
			return true
		}
	}
	newMessageID, err := sendHTMLMessage(r.token, r.chatID, text)
	if err != nil {
		newMessageID = sendPlainMessage(r.token, r.chatID, thoughtFallbackText(text))
	}
	if newMessageID == 0 {
		return false
	}
	if messageID != 0 && messageID != newMessageID {
		if err := deleteTelegramMessage(r.token, r.chatID, messageID); err != nil {
			logger.Warn("Telegram thought replacement cleanup failed: " + err.Error())
		}
	}
	return true
}

func (r *runRenderer) publishRichMessage(markdown string, messageID int) bool {
	if messageID != 0 {
		if err := editTelegramRichMessage(r.token, r.chatID, messageID, markdown); err == nil {
			return true
		}
	}
	if _, err := sendRichMessage(r.token, r.chatID, markdown); err != nil {
		return false
	}
	if messageID != 0 {
		if err := deleteTelegramMessage(r.token, r.chatID, messageID); err != nil {
			logger.Warn("Telegram replacement message cleanup failed: " + err.Error())
		}
	}
	return true
}

const telegramRichMessageLimit = 32768

func editTelegramMessage(token string, chatID int64, messageID int, text string) error {
	return postTelegramEditMessage(token, map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	})
}

// editTelegramRichMessage keeps the original payload: no reply_markup. See
// sendRichMessage for why the rich payload is treated as fixed.
func editTelegramRichMessage(token string, chatID int64, messageID int, markdown string) error {
	return postTelegramEditMessage(token, map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"rich_message": map[string]any{
			"markdown": markdown,
		},
	})
}

// attachReplyKeyboard puts the reply keyboard on a message that is already sent,
// without touching its content.
//
// This is a separate call rather than a field on the send because the message was
// created by the rich endpoint, whose payload is fixed. editMessageReplyMarkup
// changes only the markup, so the rich rendering of tool calls and thoughts is
// left alone. A rich send is the live path, so without this the run finishes with
// no keyboard at all.
func attachReplyKeyboard(token string, chatID int64, messageID int) error {
	body, _ := json.Marshal(map[string]any{
		"chat_id":      chatID,
		"message_id":   messageID,
		"reply_markup": keyboard.Main(),
	})
	resp, err := doTelegramPost(api("editMessageReplyMarkup", token), body)
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("editMessageReplyMarkup: empty response")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("editMessageReplyMarkup: HTTP %d: %s", resp.StatusCode, raw)
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("editMessageReplyMarkup response: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("editMessageReplyMarkup: %s", result.Description)
	}
	return nil
}

// editTelegramHTMLMessage carries no reply markup. Attaching a reply keyboard
// to editMessageText fails about 75% of the time, and thought messages are
// collapsible rather than carriers, so there is nothing to gain and a wasted call
// to lose on every streaming tick.
func editTelegramHTMLMessage(token string, chatID int64, messageID int, text string) error {
	return postTelegramEditMessage(token, map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
		"parse_mode": "HTML",
	})
}

func sendHTMLMessage(token string, chatID int64, text string) (int, error) {
	return sendMessagePayload(token, chatID, text, "HTML", nil)
}

func thoughtFallbackText(value string) string {
	value = strings.NewReplacer("<blockquote expandable>", "", "</blockquote>", "", "<b>", "", "</b>", "").Replace(value)
	return html.UnescapeString(value)
}

func postTelegramEditMessage(token string, payload map[string]any) error {
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

// sendRichMessage creates the assistant message through the rich endpoint.
//
// The payload is exactly chat_id plus rich_message and nothing else. Adding a
// reply_markup key here, to attach the reply keyboard, broke the endpoint's own
// rendering of tool calls and thoughts, so the keyboard is attached afterwards by
// a separate markup-only call instead. The endpoint is private and undocumented;
// treat its payload as fixed.
func sendRichMessage(token string, chatID int64, markdown string) (int, error) {
	payload := map[string]any{
		"chat_id": chatID,
		"rich_message": map[string]any{
			"markdown": markdown,
		},
	}
	body, _ := json.Marshal(payload)
	resp, err := doTelegramPost(api("sendRichMessage", token), body)
	if err != nil {
		return 0, err
	}
	if resp == nil {
		return 0, fmt.Errorf("sendRichMessage: empty response")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("sendRichMessage: HTTP %d: %s", resp.StatusCode, raw)
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, fmt.Errorf("sendRichMessage response: %w", err)
	}
	if !result.OK {
		return 0, fmt.Errorf("sendRichMessage: %s", result.Description)
	}
	return result.Result.MessageID, nil
}

func deleteTelegramMessage(token string, chatID int64, messageID int) error {
	body, _ := json.Marshal(map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	})
	resp, err := doTelegramPost(api("deleteMessage", token), body)
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("deleteMessage: empty response")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("deleteMessage: HTTP %d: %s", resp.StatusCode, raw)
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("deleteMessage response: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("deleteMessage: %s", result.Description)
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
	case "read", "write", "edit", "edit_line", "batch_edit_file", "move", "copy":
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
		return "📚 Read file"
	case "write":
		return "📝 Write file"
	case "edit", "edit_line", "batch_edit_file":
		return "📝 Edit file"
	case "bash":
		return ">_ Run command"
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

func parseToolActivity(value string) (toolActivity, bool) {
	value = strings.TrimSpace(value)
	prefixes := []struct {
		prefix string
		status string
	}{
		{"🔄 ", "🔄"},
		{"✅ ", "✓"},
		{"✓ ", "✓"},
		{"⚠️ ", "⚠"},
		{"⚠ ", "⚠"},
		{"⚡ ", "⚡"},
	}
	for _, item := range prefixes {
		if !strings.HasPrefix(value, item.prefix) {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(value, item.prefix))
		detail := ""
		if separator := strings.Index(body, " · "); separator >= 0 {
			detail = strings.TrimSpace(body[separator+3:])
			body = strings.TrimSpace(body[:separator])
		}
		if item.status != "🔄" && item.status != "⚡" {
			body = strings.TrimSpace(strings.TrimSuffix(body, " completed"))
			body = strings.TrimSpace(strings.TrimSuffix(body, " failed"))
			body = strings.TrimSpace(strings.TrimSuffix(body, " cancelled"))
		}
		return toolActivity{status: item.status, label: body, detail: detail}, true
	}
	return toolActivity{}, false
}

func formatToolActivity(tools []toolActivity) string {
	if len(tools) > 6 {
		tools = tools[len(tools)-6:]
	}
	lines := make([]string, 0, len(tools))
	for _, tool := range tools {
		line := "• " + tool.status + " " + tool.label
		if tool.detail != "" {
			line += " · " + tool.detail
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "  \n")
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
