package telegram

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"ceraa/internal/logger"
	"ceraa/internal/telegram/keyboard"
)

// Telegram has no method for setting a custom reply keyboard without sending a
// message, and a message carrying an inline keyboard replaces the custom keyboard
// for the chat. Every menu was therefore tearing the reply keyboard down, and
// is_persistent only protects the message that carried it, so deleting that
// message took the keyboard with it.
//
// The workaround is to send a throwaway message carrying the keyboard and delete
// it at once. Telegram keeps the keyboard after the message is gone, and nothing
// is left in the transcript.

// replyKeyboardDown records the chats whose reply keyboard is believed to have
// been replaced. Sending a menu marks the chat; attaching the real keyboard
// clears it, so the restore is not paid for on every message.
var replyKeyboardDown sync.Map

// keyboardRestoreText is a zero-width space: Telegram rejects empty text, and this
// is the least visible thing that satisfies it.
const keyboardRestoreText = "\u200b"

// ensureReplyKeyboard re-establishes the custom reply keyboard for a chat.
//
// Best effort throughout. A failure here costs the user a keyboard, never a
// message, so it is logged and swallowed rather than surfaced.
func ensureReplyKeyboard(token string, chatID int64) {
	replyKeyboardDown.Delete(chatID)

	payload := map[string]any{
		"chat_id":      chatID,
		"text":         keyboardRestoreText,
		"reply_markup": keyboard.Main(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	resp, err := doTelegramPost(api("sendMessage", token), body)
	if err != nil || resp == nil {
		if err != nil {
			logger.Warn("reply keyboard restore send failed: " + err.Error())
		}
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.Warn(fmt.Sprintf("reply keyboard restore send returned HTTP %d", resp.StatusCode))
		return
	}
	var sent struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &sent); err != nil || !sent.OK || sent.Result.MessageID == 0 {
		return
	}
	// The delete is what makes the restore invisible. If it fails the keyboard is
	// still set and the user is left with a blank bubble, which beats losing it.
	del, err := json.Marshal(map[string]any{"chat_id": chatID, "message_id": sent.Result.MessageID})
	if err != nil {
		return
	}
	resp, err = doTelegramPost(api("deleteMessage", token), del)
	if err == nil && resp != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// markInlineKeyboardSent records that a chat just received an inline keyboard,
// which is what displaces the reply keyboard.
func markInlineKeyboardSent(chatID int64) { replyKeyboardDown.Store(chatID, true) }

// replyKeyboardNeedsRestore reports whether a chat is believed to be missing its
// keyboard.
func replyKeyboardNeedsRestore(chatID int64) bool {
	_, down := replyKeyboardDown.Load(chatID)
	return down
}

// restoreReplyKeyboard puts the keyboard back only if the chat is believed to be
// missing it, so a run of plain messages costs nothing.
func restoreReplyKeyboard(token string, chatID int64) {
	if !replyKeyboardNeedsRestore(chatID) {
		return
	}
	ensureReplyKeyboard(token, chatID)
}

// keyboardIsInline reports whether a markup value is an inline keyboard, which is
// the one kind that displaces the custom reply keyboard.
//
// Only inline_keyboard counts. Written as "not inline and not remove" it returned
// true for the reply keyboard itself, so every plain message marked its own chat
// displaced and then paid for a restore it did not owe.
func keyboardIsInline(markup map[string]any) bool {
	if markup == nil {
		return false
	}
	_, ok := markup["inline_keyboard"]
	return ok
}
