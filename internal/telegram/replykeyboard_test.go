package telegram

import (
	"strings"
	"testing"

	"ceraa/internal/config"
	"ceraa/internal/telegram/menus"
)

// testConfig is a minimal config for exercising command paths. The keyboard work
// happens before any provider is consulted, so an unconfigured provider (and
// therefore the onboarding menu) is fine here.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{}
}

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

// keyboardsSent counts the messages that carried the custom reply keyboard.
func (c *capture) keyboardsSent() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, call := range c.calls {
		markup, ok := call.Body["reply_markup"].(map[string]any)
		if !ok {
			continue
		}
		if _, isReply := markup["keyboard"]; isReply {
			count++
		}
	}
	return count
}

// The defect: a message carrying an inline keyboard replaces the custom reply
// keyboard for the chat, so every menu silently removed it, and is_persistent only
// protects the message that carried it, so deleting that message took it away too.
func TestSendingAMenuRestoresTheReplyKeyboard(t *testing.T) {
	cap := beginCapture(t)
	sendMenuMarkup("test-token", 101, "menu", menus.Hub())

	methods := cap.methods()
	if !calledMethod(methods, "deleteMessage") {
		t.Fatalf("a menu did not restore the keyboard; calls were %v", methods)
	}
	if cap.keyboardsSent() == 0 {
		t.Fatalf("a menu did not send the reply keyboard at all; calls were %v", methods)
	}
}

// The restore has to be invisible, so the message carrying the keyboard must be
// deleted. That is also what makes the keyboard independent of any single message
// surviving: once restored, no live message holds it, so deleting messages cannot
// take it away. That is the whole point, and the reason a guard on the original
// message was never enough.
func TestTheRestoringMessageIsDeletedSoNoMessageHoldsTheKeyboard(t *testing.T) {
	cap := beginCapture(t)
	sendMenuMarkup("test-token", 102, "menu", menus.Hub())

	cap.mu.Lock()
	defer cap.mu.Unlock()
	keyboardMessage, deleted := -1, -1
	for i, call := range cap.calls {
		markup, ok := call.Body["reply_markup"].(map[string]any)
		if ok {
			if _, isReply := markup["keyboard"]; isReply {
				keyboardMessage = i
			}
		}
		if strings.Contains(call.Method, "deleteMessage") {
			deleted = i
		}
	}
	if keyboardMessage < 0 {
		t.Fatal("no message carried the reply keyboard")
	}
	if deleted < 0 {
		t.Fatal("the message carrying the reply keyboard was never deleted")
	}
	if deleted != keyboardMessage+1 {
		t.Errorf("the keyboard message was not deleted straight after being sent (message at %d, delete at %d)", keyboardMessage, deleted)
	}
}

// A plain message already carries the keyboard, so nothing is owed and the restore
// must not fire. Otherwise every reply would cost two extra API calls.
func TestAPlainMessageDoesNotTriggerARestore(t *testing.T) {
	cap := beginCapture(t)
	send("test-token", 103, "hello")

	if calledMethod(cap.methods(), "deleteMessage") {
		t.Errorf("a plain message triggered a keyboard restore: %v", cap.methods())
	}
	if cap.keyboardsSent() == 0 {
		t.Error("a plain message did not carry the reply keyboard")
	}
}

// A run of plain messages after a menu must not keep paying for the restore.
func TestTheRestoreIsPaidOncePerDisplacement(t *testing.T) {
	cap := beginCapture(t)
	sendMenuMarkup("test-token", 104, "menu", menus.Hub())
	// The restore is the send-then-delete pair, so the delete count is the number
	// of restores. Each plain message legitimately carries the keyboard itself, so
	// counting keyboards would measure the wrong thing.
	afterMenu := 0
	for _, method := range cap.methods() {
		if strings.Contains(method, "deleteMessage") {
			afterMenu++
		}
	}
	if afterMenu != 1 {
		t.Fatalf("the menu did not restore exactly once, got %d", afterMenu)
	}

	send("test-token", 104, "one")
	send("test-token", 104, "two")
	restores := 0
	for _, method := range cap.methods() {
		if strings.Contains(method, "deleteMessage") {
			restores++
		}
	}
	if restores != afterMenu {
		t.Errorf("plain messages kept triggering restores: %d then %d", afterMenu, restores)
	}
}

// /start is the recovery entry point: someone who has lost the keyboard types
// it. It ends with a menu, and the menu path restores the keyboard, so the
// end state is "present" without any extra force. Asserting the end state rather
// than a particular mechanism matters here: an earlier version forced a restore
// first and then immediately displaced it again, so the force cost two API calls
// per /start and changed nothing observable.
func TestStartLeavesTheKeyboardPresent(t *testing.T) {
	cap := beginCapture(t)
	handleStart("test-token", 105, testConfig(t))

	if replyKeyboardNeedsRestore(105) {
		t.Error("/start left the chat marked displaced after the menu it sent")
	}
	if cap.keyboardsSent() == 0 {
		t.Errorf("/start did not leave the user with a keyboard: %v", cap.methods())
	}
}

// After any restore the chat must be considered healthy, or the next message would
// pay for it again.
func TestRestoreClearsTheDisplacedFlag(t *testing.T) {
	beginCapture(t)
	markInlineKeyboardSent(202)
	if !replyKeyboardNeedsRestore(202) {
		t.Fatal("an inline send did not mark the chat as needing a restore")
	}
	ensureReplyKeyboard("test-token", 202)
	if replyKeyboardNeedsRestore(202) {
		t.Error("the chat still counts as displaced after a restore")
	}
}

func TestKeyboardIsInlineDetection(t *testing.T) {
	cases := []struct {
		name   string
		markup map[string]any
		want   bool
	}{
		{"nil", nil, false},
		{"inline", map[string]any{"inline_keyboard": [][]any{}}, true},
		{"remove", map[string]any{"remove_keyboard": true}, false},
		{"reply", map[string]any{"keyboard": [][]any{}}, false},
	}
	for _, tc := range cases {
		if got := keyboardIsInline(tc.markup); got != tc.want {
			t.Errorf("%s: keyboardIsInline = %v, want %v", tc.name, got, tc.want)
		}
	}
}
