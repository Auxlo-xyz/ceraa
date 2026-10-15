package telegram

import (
	"strings"
	"testing"

	"ceraa/internal/telegram/keyboard"
	"ceraa/internal/telegram/menus"
)

// Telegram has no way to set a custom reply keyboard without sending a message,
// and the keyboard only exists while some message carrying it survives. Deleting
// the messages that carry it removes the keyboard, and is_persistent does not stop
// that: it only asks the client to keep showing the keyboard when the regular
// on-screen keyboard is hidden.
//
// That makes the keyboard hostage to message retention, so the policy here is
// deliberately the cheap half of the fix: re-assert the keyboard on every plain
// reply and never leave anything in the transcript that has to be cleaned up.
//
// The rejected alternative was sending a throwaway zero-width message to hold the
// keyboard and deleting it at once. It looked like it worked, and it was worse
// than useless: the delete removed the very message the keyboard was riding on, so
// it caused the disappearance it was meant to prevent.

// methods returns the API methods called, in order.
func (c *capture) methods() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.calls))
	for _, call := range c.calls {
		out = append(out, call.Method)
	}
	return out
}

func calledMethod(methods []string, want string) bool {
	for _, method := range methods {
		if strings.Contains(method, want) {
			return true
		}
	}
	return false
}

// sends returns the outbound message-creating calls, in order.
//
// Both sendMessage and sendRichMessage create a user-visible message, and the rich
// endpoint is the one production actually uses. Filtering to sendMessage alone hid
// the creation of the assistant message, which is how a green suite came to
// validate a fallback branch that never runs.
func (c *capture) sends() []capturedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedCall
	for _, call := range c.calls {
		if strings.HasSuffix(call.Method, "sendMessage") || strings.HasSuffix(call.Method, "sendRichMessage") {
			out = append(out, call)
		}
	}
	return out
}

// carriesReplyKeyboard reports whether a call's markup is the custom reply keyboard.
func carriesReplyKeyboard(body map[string]any) bool {
	markup, ok := body["reply_markup"].(map[string]any)
	if !ok {
		return false
	}
	_, isReply := markup["keyboard"]
	return isReply
}

// A plain reply is what re-asserts the keyboard, so it must always carry it. If
// this regresses the keyboard silently stops coming back after a deletion, with no
// error anywhere to notice.
func TestPlainRepliesCarryTheReplyKeyboard(t *testing.T) {
	cap := beginCapture(t)
	send("test-token", 1, "here is your answer")

	sends := cap.sends()
	if len(sends) != 1 {
		t.Fatalf("expected one message, got %d: %v", len(sends), cap.methods())
	}
	if !carriesReplyKeyboard(sends[0].Body) {
		t.Error("a plain reply did not carry the reply keyboard")
	}
}

// Every send must be deliberate. An empty or zero-width carrier is the artifact
// this policy exists to avoid, because it either litters the transcript or, if it
// is deleted again, takes the keyboard down with it.
func TestNoReplyIsEmptyOrZeroWidth(t *testing.T) {
	cap := beginCapture(t)
	send("test-token", 1, "answer")
	sendMenuMarkup("test-token", 1, "menu", menus.Hub())

	for _, call := range cap.sends() {
		text, _ := call.Body["text"].(string)
		trimmed := strings.TrimSpace(text)
		trimmed = strings.Trim(trimmed, "\u200b\u200c\u200d\ufeff")
		if trimmed == "" {
			t.Errorf("sent a message with no visible content: %q", text)
		}
	}
}

// A menu cannot carry the reply keyboard, because reply_markup is one of four
// mutually exclusive kinds. So the menu is the documented gap: the keyboard is
// absent until the next plain reply. What must not happen is the bot papering over
// that gap with a second, throwaway message.
func TestAMenuDoesNotSendAThrowawayKeyboardCarrier(t *testing.T) {
	cap := beginCapture(t)
	sendMenuMarkup("test-token", 1, "menu", menus.Hub())

	sends := cap.sends()
	if len(sends) != 1 {
		t.Errorf("a menu sent %d messages, want 1: %v", len(sends), cap.methods())
	}
	if calledMethod(cap.methods(), "deleteMessage") {
		t.Errorf("a menu deleted a message to clean up after itself: %v", cap.methods())
	}
	// And the one message it does send has to be the menu. Swapping the inline
	// keyboard for the reply keyboard would also leave a single, non-deleted
	// message, and would quietly cost every button.
	if len(sends) == 1 {
		markup, _ := sends[0].Body["reply_markup"].(map[string]any)
		if _, isInline := markup["inline_keyboard"]; !isInline {
			t.Errorf("the menu lost its inline keyboard: %#v", sends[0].Body["reply_markup"])
		}
	}
}

// The client-side guarantee we do rely on, pinned by a test so it cannot be
// dropped from the markup by accident. Note what it does and does not buy:
// is_persistent asks the client to keep showing the keyboard when the regular
// on-screen keyboard is hidden. It does not survive deletion of the message that
// carried it, which is the whole reason for the policy above.
func TestTheReplyKeyboardIsPersistent(t *testing.T) {
	markup := keyboard.Main()
	if markup["is_persistent"] != true {
		t.Errorf("is_persistent = %#v, want true", markup["is_persistent"])
	}
	rows, ok := markup["keyboard"].([][]map[string]string)
	if !ok {
		t.Fatalf("keyboard.Main() has unexpected keyboard rows: %#v", markup["keyboard"])
	}
	if len(rows) == 0 {
		t.Fatal("the reply keyboard has no rows")
	}
	for i, row := range rows {
		if len(row) == 0 {
			t.Errorf("row %d is empty", i)
		}
		for j, key := range row {
			if strings.TrimSpace(key["text"]) == "" {
				t.Errorf("row %d key %d has no label", i, j)
			}
		}
	}
}

// The answer to an actual prompt is the message a user reads, and it used to be
// the one reply sent with no keyboard at all: send() attached it, but the agent
// answer goes out through the streaming renderer and the plain-text fallback,
// which both passed nil markup. So /help looked correct while the real answer had
// no keyboard. This is the regression that produced a screenshot of a perfectly
// good reply sitting on top of a missing keyboard.
func TestTheAgentAnswerCarriesTheReplyKeyboard(t *testing.T) {
	cap := beginCapture(t)
	sendPlainMessage("test-token", 1, "here is the answer to your prompt")

	sends := cap.sends()
	if len(sends) != 1 {
		t.Fatalf("expected one message, got %d: %v", len(sends), cap.methods())
	}
	if !carriesReplyKeyboard(sends[0].Body) {
		t.Error("the agent's answer did not carry the reply keyboard")
	}
}

// A run creates its assistant message through the rich endpoint, whose payload
// cannot carry markup. The keyboard used to be attached afterwards with a
// markup-only editMessageReplyMarkup call, but that endpoint accepts only an
// inline keyboard and answers "inline keyboard expected" to a reply keyboard
// on every call (verified live), so the attach could never succeed and only
// produced 400s in the log. The keyboard travels on sends only and persists
// client-side across messages.
//
// The creation is lazy: there is no message until the run has something to put in
// it, because creating one up front left an empty cursor bubble at the top of every
// run.
//
// This test pins the absence: no editMessageReplyMarkup call in a run may
// carry a reply keyboard.
func TestARunMakesNoDoomedKeyboardEdit(t *testing.T) {
	cap := beginCapture(t)
	renderer := newRunRenderer("test-token", 1)
	renderer.content("the answer")
	renderer.finish("the answer")
	renderer.close()

	for _, call := range cap.calls {
		if !strings.HasSuffix(call.Method, "editMessageReplyMarkup") {
			continue
		}
		if markup, _ := call.Body["reply_markup"].(map[string]any); markup != nil {
			if _, isReply := markup["keyboard"]; isReply {
				t.Error("editMessageReplyMarkup carried a reply keyboard, which the Bot API always rejects")
			}
		}
	}
}

// Every carrier is a reason the keyboard survives. The keyboard is only lost when
// all messages carrying it are deleted, so spreading it across the command reply,
// the answer, and the assistant message means deleting any one message cannot take
// it away. This is what makes the keyboard effectively deletion-proof without
// needing a deletion event, which Telegram does not send to bots.
func TestEveryUserFacingReplyIsACarrier(t *testing.T) {
	cap := beginCapture(t)
	send("test-token", 1, "command reply")
	sendPlainMessage("test-token", 1, "answer to the prompt")
	renderer := newRunRenderer("test-token", 1)
	renderer.content("the streamed answer")
	renderer.finish("the streamed answer")
	renderer.close()

	carriers := 0
	for _, call := range cap.calls {
		if carriesReplyKeyboard(call.Body) {
			carriers++
		}
		if strings.HasSuffix(call.Method, "editMessageReplyMarkup") {
			if markup, _ := call.Body["reply_markup"].(map[string]any); markup != nil {
				if _, isReply := markup["keyboard"]; isReply {
					t.Error("a reply keyboard went out on an edit, which the Bot API always rejects; keyboards travel on sends only")
				}
			}
		}
	}
	if carriers < 2 {
		t.Errorf("only %d of 2 sends carry the keyboard, so deleting the rest would still take it away: %v",
			carriers, cap.methods())
	}
}

// /keyboard exists because the Bot API has no way to flush what a client has
// cached about a chat UI. Removal followed by a fresh keyboard is the documented
// way to make a client rebuild one, and it is the only lever available.
//
// The order is the entire mechanism: setting the keyboard again while the client
// believes it already has one can be a no-op, which is exactly the stuck state
// this clears. Asserted as a sequence so swapping the two fails.
func TestKeyboardResetRemovesBeforeReinstating(t *testing.T) {
	cap := beginCapture(t)
	handleKeyboard("test-token", 1)

	var sawRemoval, sawKeyboard bool
	order := 0
	for _, call := range cap.sends() {
		markup, _ := call.Body["reply_markup"].(map[string]any)
		if _, removing := markup["remove_keyboard"]; removing {
			sawRemoval = true
			if order > 0 {
				t.Fatal("the removal was sent after the keyboard was set, so the client would treat the reset as a no-op")
			}
			order++
			continue
		}
		if _, isReply := markup["keyboard"]; isReply {
			sawKeyboard = true
			order++
		}
	}
	if !sawRemoval {
		t.Errorf("no ReplyKeyboardRemove was sent: %v", cap.methods())
	}
	if !sawKeyboard {
		t.Errorf("the keyboard was not reinstated: %v", cap.methods())
	}
}

// The removal message is bookkeeping and must not be left in the transcript. It
// carries no keyboard, so deleting it cannot take the keyboard down with it.
func TestKeyboardResetCleansUpTheRemovalMessage(t *testing.T) {
	cap := beginCapture(t)
	handleKeyboard("test-token", 1)

	if !calledMethod(cap.methods(), "deleteMessage") {
		t.Errorf("the reset left its removal message in the transcript: %v", cap.methods())
	}
}

// The reinstatement is a real user-facing reply, so it must carry the keyboard
// and must not be one of the invisible placeholder messages.
func TestKeyboardResetEndsWithAVisibleCarryingMessage(t *testing.T) {
	cap := beginCapture(t)
	handleKeyboard("test-token", 1)

	sends := cap.sends()
	if len(sends) == 0 {
		t.Fatal("the reset sent nothing")
	}
	last := sends[len(sends)-1]
	if !carriesReplyKeyboard(last.Body) {
		t.Error("the final message of the reset did not carry the keyboard")
	}
	text, _ := last.Body["text"].(string)
	if strings.TrimSpace(strings.Trim(text, "\u200b")) == "" {
		t.Errorf("the reset told the user nothing: %q", text)
	}
}

// Attaching a reply keyboard to an edit fails on every call in production
// ("inline keyboard expected"), while a fresh send succeeds every time. So
// edits carry no markup, and the keyboard rides on sends only.
//
// These tests assert the absence on purpose: the previous version of this file
// asserted the opposite, pinning the bug in place and costing every streaming
// run wasted 400 round trips.
func TestContentEditsCarryNoReplyMarkup(t *testing.T) {
	cap := beginCapture(t)
	if err := editTelegramMessage("test-token", 1, 42, "the final answer"); err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	if err := editTelegramHTMLMessage("test-token", 1, 42, "<b>answer</b>"); err != nil {
		t.Fatalf("html edit failed: %v", err)
	}

	edits := 0
	for _, call := range cap.calls {
		if strings.HasSuffix(call.Method, "editMessageText") {
			edits++
			if _, present := call.Body["reply_markup"]; present {
				t.Errorf("editMessageText carries reply_markup, which the Bot API rejects: %#v", call.Body["reply_markup"])
			}
		}
	}
	if edits != 2 {
		t.Fatalf("expected 2 edits, got %d: %v", edits, cap.methods())
	}
}

// The rich payload is exactly chat_id plus rich_message. Adding reply_markup to it
// attached the keyboard but broke the endpoint's own rendering of tool calls and
// thoughts, so the payload is treated as fixed and the keyboard is attached by a
// separate markup-only call instead.
//
// This is the regression guard for that: it fails if anyone puts markup back into a
// rich send.
func TestTheRichSendPayloadStaysClean(t *testing.T) {
	cap := beginCapture(t)
	if _, err := sendRichMessage("test-token", 1, "the answer"); err != nil {
		t.Fatalf("sendRichMessage failed: %v", err)
	}

	var rich map[string]any
	for _, call := range cap.calls {
		if strings.HasSuffix(call.Method, "sendRichMessage") {
			rich = call.Body
		}
	}
	if rich == nil {
		t.Fatalf("no sendRichMessage was sent: %v", cap.methods())
	}
	if _, present := rich["reply_markup"]; present {
		t.Error("the rich send carries reply_markup, which breaks its tool call and thought rendering")
	}
	if len(rich) != 2 {
		t.Errorf("the rich payload has %d keys, want exactly chat_id and rich_message: %#v", len(rich), rich)
	}
}

func TestTheRichEditPayloadStaysClean(t *testing.T) {
	cap := beginCapture(t)
	_ = editTelegramRichMessage("test-token", 1, 42, "the answer")

	var edit map[string]any
	for _, call := range cap.calls {
		if strings.HasSuffix(call.Method, "editMessageText") {
			edit = call.Body
		}
	}
	if edit == nil {
		t.Fatalf("no editMessageText was sent: %v", cap.methods())
	}
	if _, present := edit["reply_markup"]; present {
		t.Error("the rich edit carries reply_markup, which breaks its rendering")
	}
}

// The keyboard used to reach a rich-created message through
// editMessageReplyMarkup. That endpoint accepts only an inline keyboard, so the
// call failed on every run and the keyboard never arrived that way. Coverage
// comes from sends: every plain send carries the keyboard and it persists
// client-side. This test pins that no rich-created run attempts the dead edit.
func TestARichRunMakesNoDoomedKeyboardEdit(t *testing.T) {
	cap := beginCapture(t)
	renderer := newRunRenderer("test-token", 1)
	renderer.content("the streamed answer")
	renderer.finish("the streamed answer")
	renderer.close()

	for _, call := range cap.calls {
		if !strings.HasSuffix(call.Method, "editMessageReplyMarkup") {
			continue
		}
		if markup, _ := call.Body["reply_markup"].(map[string]any); markup != nil {
			if _, isReply := markup["keyboard"]; isReply {
				t.Error("editMessageReplyMarkup carried a reply keyboard, which the Bot API always rejects")
			}
		}
	}
}

// The final answer is the message the user reads. It used to end with a
// markup-only editMessageReplyMarkup carrying the reply keyboard, which the
// Bot API rejects on every call. The keyboard travels on sends only, so this
// pins that the final-answer path makes no such call.
func TestTheFinalAnswerEndsWithTheKeyboardAttached(t *testing.T) {
	cap := beginCapture(t)
	r := &runRenderer{token: "rich-fail-test-token", chatID: 1}
	r.publishFinalAnswer("the final answer", 42)

	for _, call := range cap.calls {
		if !strings.HasSuffix(call.Method, "editMessageReplyMarkup") {
			continue
		}
		if markup, _ := call.Body["reply_markup"].(map[string]any); markup != nil {
			if _, isReply := markup["keyboard"]; isReply {
				t.Error("editMessageReplyMarkup carried a reply keyboard, which the Bot API always rejects")
			}
		}
	}
}

// When there is no existing message, the answer is a fresh send, which carries
// the keyboard on its own. Asserted so a future refactor cannot route this case
// through an edit that strips it.
func TestTheFinalAnswerWithoutAMessageIsAFreshSend(t *testing.T) {
	cap := beginCapture(t)
	r := &runRenderer{token: "rich-fail-test-token", chatID: 1}
	r.publishFinalAnswer("the final answer", 0)

	sends := cap.sends()
	if len(sends) == 0 {
		t.Fatalf("the answer was never sent: %v", cap.methods())
	}
	if !carriesReplyKeyboard(sends[len(sends)-1].Body) {
		t.Error("the fresh-send fallback did not carry the keyboard")
	}
}
