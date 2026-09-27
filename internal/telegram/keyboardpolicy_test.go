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

// The run renderer creates the assistant message once and then only ever edits it.
// Telegram keeps the existing reply_markup when an edit omits it, so attaching the
// keyboard at creation is what carries it through the streamed text and the final
// answer. If this regresses, every run finishes with a keyboard-less message even
// though the first frame had one.
func TestTheRunRendererAttachesTheKeyboardAtCreation(t *testing.T) {
	cap := beginCapture(t)
	newRunRenderer("test-token", 1)

	sends := cap.sends()
	if len(sends) == 0 {
		t.Fatalf("the renderer sent nothing: %v", cap.methods())
	}
	// The rich endpoint is what production takes, so the keyboard has to be on
	// whichever call actually created the message.
	if !carriesReplyKeyboard(sends[0].Body) {
		t.Errorf("the assistant message was created without the reply keyboard: %s", cap.methods()[0])
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
	newRunRenderer("test-token", 1)

	carriers := 0
	for _, call := range cap.sends() {
		if carriesReplyKeyboard(call.Body) {
			carriers++
		}
	}
	if carriers < 3 {
		t.Errorf("only %d of 3 user-facing messages carry the keyboard, so deleting the rest would still take it away: %v",
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

// An edit replaces a message's reply_markup rather than inheriting it. The
// keyboard was attached when the assistant message was created, and the final
// answer is an edit of that same message, so an edit without reply_markup
// stripped the keyboard a moment after it was set.
//
// This is why /keyboard appeared to work while prompts did not: the keyboard
// command finishes with a fresh send, and nothing strips a fresh send, whereas
// the answer is written by an edit. Asserted on the edit itself, since that is
// the call that decides whether the answer keeps the keyboard.
func TestTheFinalAnswerEditCarriesTheReplyKeyboard(t *testing.T) {
	cap := beginCapture(t)
	if err := editTelegramMessageWithKeyboard("test-token", 1, 42, "the final answer"); err != nil {
		t.Fatalf("edit failed: %v", err)
	}

	var edit map[string]any
	for _, call := range cap.calls {
		if strings.HasSuffix(call.Method, "editMessageText") {
			edit = call.Body
		}
	}
	if edit == nil {
		t.Fatalf("no editMessageText was sent: %v", cap.methods())
	}
	markup, _ := edit["reply_markup"].(map[string]any)
	if _, isReply := markup["keyboard"]; !isReply {
		t.Error("the edit publishing the final answer carries no reply keyboard, so it strips the one set at creation")
	}
}

// The HTML edit has the same defect and is the path used when the answer needs
// formatting.
func TestTheHTMLEditCarriesTheReplyKeyboard(t *testing.T) {
	cap := beginCapture(t)
	if err := editTelegramHTMLMessage("test-token", 1, 42, "<b>answer</b>"); err != nil {
		t.Fatalf("edit failed: %v", err)
	}

	var edit map[string]any
	for _, call := range cap.calls {
		if strings.HasSuffix(call.Method, "editMessageText") {
			edit = call.Body
		}
	}
	if edit == nil {
		t.Fatalf("no editMessageText was sent: %v", cap.methods())
	}
	markup, _ := edit["reply_markup"].(map[string]any)
	if _, isReply := markup["keyboard"]; !isReply {
		t.Error("the HTML edit strips the reply keyboard")
	}
}

// The rich endpoint is the branch production takes, and it was the one carrying no
// reply keyboard. Every earlier test passed because the stub rejected
// sendRichMessage, so this path was never exercised; the trace is what showed the
// endpoint works and the branch is live.
//
// Pinned directly so removing the markup from the rich send fails here rather than
// in production.
func TestTheRichSendCarriesTheReplyKeyboard(t *testing.T) {
	cap := beginCapture(t)
	if _, err := sendRichMessage("test-token", 1, "the answer", keyboard.Main()); err != nil {
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
	markup, _ := rich["reply_markup"].(map[string]any)
	if _, isReply := markup["keyboard"]; !isReply {
		t.Error("the rich send carries no reply keyboard, and this is the path a real prompt takes")
	}
}

// The rich edit replaces the message's markup like any other edit, so it needs the
// keyboard too. The API currently rejects rich edits outright, but the fallback
// path calls this whenever it is accepted, so it cannot be left without markup.
func TestTheRichEditCarriesTheReplyKeyboard(t *testing.T) {
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
	markup, _ := edit["reply_markup"].(map[string]any)
	if _, isReply := markup["keyboard"]; !isReply {
		t.Error("the rich edit strips the reply keyboard")
	}
}

// A renderer that reaches production must create its assistant message through the
// rich endpoint with the keyboard attached. This is the exact shape of the bug the
// trace caught: the keyboard was on the fallback branch only.
func TestProductionCreationPathCarriesTheKeyboard(t *testing.T) {
	cap := beginCapture(t)
	newRunRenderer("test-token", 1)

	var rich map[string]any
	for _, call := range cap.calls {
		if strings.HasSuffix(call.Method, "sendRichMessage") {
			rich = call.Body
		}
	}
	if rich == nil {
		t.Fatalf("production took the fallback branch, so this test is not covering the live path: %v", cap.methods())
	}
	markup, _ := rich["reply_markup"].(map[string]any)
	if _, isReply := markup["keyboard"]; !isReply {
		t.Error("the live creation path attaches no reply keyboard")
	}
}
