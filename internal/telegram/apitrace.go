package telegram

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"ceraa/internal/logger"
)

// Telegram gives no way to ask a client what it is displaying, and no way to read
// back a chat's reply keyboard. The only way to find out whether a keyboard was
// actually delivered is to record what was sent and what the API answered, so
// this traces every outbound call.
//
// It exists because the previous failures were invisible. The bot logged warnings
// only when a call failed, so a call that returned ok:true and was then ignored by
// the client produced no log line at all, and the only evidence was a screenshot.

// telegramTrace records every API call to the log.
//
// On by default for now. The reply keyboard failures being chased leave no trace
// at all when a call succeeds and a client ignores it, so a chat that misbehaves
// silently produced an empty log and the only evidence was a screenshot. It logs
// one line per call, so /apilog off turns it down once the cause is found.
var telegramTrace atomic.Bool

func init() { telegramTrace.Store(true) }

// traceObserver is a test hook for reading trace lines. Declared here and set only
// by tests, so production pays one nil check and the log stays the only sink.
var traceObserver atomic.Pointer[func(string)]

// SetTelegramTrace turns API tracing on or off at runtime.
func SetTelegramTrace(on bool) { telegramTrace.Store(on) }

// TelegramTraceEnabled reports whether API tracing is on.
func TelegramTraceEnabled() bool { return telegramTrace.Load() }

// markupKind names the reply_markup in a payload, which is the part that decides
// whether a message carries a reply keyboard, an inline keyboard, or neither.
func markupKind(body []byte) string {
	var payload struct {
		ReplyMarkup map[string]any `json:"reply_markup"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "unparseable"
	}
	markup := payload.ReplyMarkup
	switch {
	case markup == nil:
		return "none"
	case hasKey(markup, "keyboard"):
		return "reply_keyboard"
	case hasKey(markup, "inline_keyboard"):
		return "inline_keyboard"
	case hasKey(markup, "remove_keyboard"):
		return "remove_keyboard"
	case hasKey(markup, "force_reply"):
		return "force_reply"
	default:
		return "unknown"
	}
}

func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

// traceAPICall records one outbound call and the API's verdict on it.
//
// The verdict is the part that matters. ok:true only means Telegram accepted the
// request, not that a client rendered it, so the response description is logged
// alongside it: a silent strip and a success look identical from the bot's side
// otherwise.
func traceAPICall(method string, body []byte, status int, response string, apiOK bool, description string) {
	if !telegramTrace.Load() {
		return
	}
	kind := markupKind(body)
	var payload struct {
		Text    string `json:"text"`
		RichMsg struct {
			Markdown string `json:"markdown"`
		} `json:"rich_message"`
	}
	_ = json.Unmarshal(body, &payload)
	textLen := len([]rune(payload.Text))
	// A rich call carries its body in rich_message.markdown and leaves text
	// empty, so measuring only text reported 0 for every rich send and edit. That
	// made the trace unable to answer the one question it exists for, which is
	// what a message actually holds.
	markdownLen := len([]rune(payload.RichMsg.Markdown))
	body_ := payload.Text
	if body_ == "" {
		body_ = payload.RichMsg.Markdown
	}
	verdict := "ok"
	switch {
	case !apiOK:
		verdict = "REJECTED: " + description
	case status < 200 || status >= 300:
		verdict = fmt.Sprintf("HTTP %d", status)
	}
	line := fmt.Sprintf("tgapi %s markup=%s textlen=%d mdlen=%d preview=%q verdict=%s raw=%s",
		method, kind, textLen, markdownLen, previewForTrace(body_), verdict, truncate(response, 200))
	if hook := traceObserver.Load(); hook != nil {
		(*hook)(line)
	}
	logger.Warn(line)
}

// previewForTrace is one line of the body a call carries, so a trace can be read
// without reconstructing it. Newlines are folded rather than dropped: the content
// of a message is largely where its line breaks are.
func previewForTrace(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\n", "\\n")
	runes := []rune(value)
	if len(runes) > 110 {
		return string(runes[:110]) + "..."
	}
	return value
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// handleAPILog toggles API tracing and reports the new state.
//
// Toggling at runtime matters because the failures being chased are intermittent
// and depend on client state: a trace left on forever would bury the log, and a
// trace that needs a redeploy cannot be turned on at the moment the keyboard goes
// missing.
func handleAPILog(token string, chatID int64, arg string) {
	switch strings.TrimSpace(arg) {
	case "on", "1", "true", "start":
		SetTelegramTrace(true)
	case "off", "0", "false", "stop":
		SetTelegramTrace(false)
	}
	state := "off"
	if TelegramTraceEnabled() {
		state = "on"
	}
	send(token, chatID, "🔎 Telegram API tracing is "+state+". Every call now logs its method, reply_markup kind, and whether the API accepted it. Use /apilog off to stop.")
}
