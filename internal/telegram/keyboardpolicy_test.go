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

// sends returns the outbound sendMessage calls, in order.
func (c *capture) sends() []capturedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedCall
	for _, call := range c.calls {
		if strings.HasSuffix(call.Method, "sendMessage") {
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
