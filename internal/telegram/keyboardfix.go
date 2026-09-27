package telegram

import (
	"encoding/json"
	"io"
	"strconv"

	"ceraa/internal/logger"
)

// The Telegram Bot API has no endpoint for flushing what a client has cached
// about a chat's reply keyboard. There is nothing to invalidate server-side, and
// no sequence of API calls that reaches into a phone and clears its UI state.
//
// What does work is the documented way of making a client rebuild a keyboard:
// send ReplyKeyboardRemove, then send the keyboard again. Tearing the keyboard
// down and putting it back forces the client to re-read it, which is the closest
// available equivalent of a cache flush. It is exposed as an explicit /keyboard
// command rather than done on every reply, because it costs two visible messages
// and should not be paid for by ordinary conversation.

// removeKeyboardMarkup is ReplyKeyboardRemove, which takes the keyboard down.
func removeKeyboardMarkup() map[string]any {
	return map[string]any{"remove_keyboard": true}
}

// sendKeyboardRemoval takes the reply keyboard down for a chat.
func sendKeyboardRemoval(token string, chatID int64) int {
	body, err := json.Marshal(map[string]any{
		"chat_id":      chatID,
		"text":         keyboardRestorePlaceholder,
		"reply_markup": removeKeyboardMarkup(),
	})
	if err != nil {
		return 0
	}
	resp, err := doTelegramPost(api("sendMessage", token), body)
	if err != nil || resp == nil {
		if err != nil {
			logger.Warn("keyboard removal send failed: " + err.Error())
		}
		return 0
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.Warn("keyboard removal returned HTTP " + strconv.Itoa(resp.StatusCode))
		return 0
	}
	var sent struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &sent); err != nil || !sent.OK || sent.Result.MessageID == 0 {
		return 0
	}
	// The removal message is invisible once deleted, and deleting it is what keeps
	// the transcript clean. It carries no keyboard, so removing it cannot take a
	// keyboard down.
	_ = deleteTelegramMessage(token, chatID, sent.Result.MessageID)
	return sent.Result.MessageID
}

// keyboardRestorePlaceholder is the text used for the invisible bookkeeping
// messages here. Telegram rejects empty text, so this is the least visible thing
// that satisfies it.
const keyboardRestorePlaceholder = "\u200b"

// handleKeyboard forces the client to rebuild the reply keyboard, for when it is
// visibly absent even though the API has been sending it.
//
// The order matters and is the whole trick: remove first, then set. Setting the
// keyboard again while it is already up can be treated as a no-op by a client that
// believes it already has it, which is exactly the stuck state this is meant to
// clear.
func handleKeyboard(token string, chatID int64) {
	sendKeyboardRemoval(token, chatID)
	if id := sendPlainMessage(token, chatID, "⌨️ Keyboard reset. If it is still missing, reopen the chat."); id == 0 {
		send(token, chatID, "⌨️ Keyboard reset. If it is still missing, reopen the chat.")
	}
}
