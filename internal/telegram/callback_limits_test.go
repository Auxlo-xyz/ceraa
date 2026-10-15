package telegram

import (
	"strings"
	"testing"
	"time"
)

// Telegram rejects the entire message when any callback_data exceeds 64 bytes.
// Not the button: the message. So an over-long button deletes the menu, and a
// permission prompt that cannot be shown is worse than no prompt at all, because
// the tool then blocks for its full five minute timeout and the run reads as a
// stalled model.
//
// Both of these shipped broken and were only found by reading production logs.
// The values below are the real ones from a live session, not invented ones.
const telegramCallbackDataLimit = 64

// A provider call id is 41 bytes for xAI-style ids, which is most of the budget
// before anything else is added.
const realisticCallID = "call-6fe2f5d5-1308-4908-b519-7024dfe9e8fc-107"

func assertUnderLimit(t *testing.T, what, data string) {
	t.Helper()
	if n := len(data); n > telegramCallbackDataLimit {
		t.Errorf("%s callback_data is %d bytes, over Telegram's %d: %q",
			what, n, telegramCallbackDataLimit, data)
	}
}

// These assert on the keyboards the event handler actually sends, not on a
// reimplementation of them, so a change that inlines long data again fails here.
func TestRenderedPermissionKeyboardFitsTelegram(t *testing.T) {
	kb := permissionKeyboard(pendingCallback{
		SessionID: "tg-test", RequestID: realisticCallID, Tool: "batch_edit_file",
	})
	assertKeyboardUnderLimit(t, "permission", kb)
}

func TestRenderedQuestionKeyboardFitsTelegram(t *testing.T) {
	options := []string{
		"Run `git read-tree HEAD` to rebuild the index from HEAD's tree, then stage the README and continue.",
		"Skip the index fix — re-read `README.md` to confirm the rewrite is on disk, then you (or a human operator) will repair `.git/index` and commit manually.",
	}
	assertKeyboardUnderLimit(t, "question, options", questionKeyboard(
		pendingCallback{SessionID: "tg-test", RequestID: realisticCallID}, options, true))
	assertKeyboardUnderLimit(t, "question, free-form only", questionKeyboard(
		pendingCallback{SessionID: "tg-test", RequestID: realisticCallID}, nil, true))
}

// Options are suggestions, never the only allowed answers: the type-an-answer
// row is unconditional, even on a plain choice with free_form off. Tapping it
// marks the next plain message as the answer, so a value, a path, or a name
// outside the options is always reachable.
func TestQuestionKeyboardAlwaysOffersTyping(t *testing.T) {
	kb := questionKeyboard(
		pendingCallback{SessionID: "tg-test", RequestID: realisticCallID},
		[]string{"Restart now", "Remind me later"}, false)
	rows, ok := kb["inline_keyboard"].([][]map[string]string)
	if !ok {
		t.Fatalf("question keyboard has no rows: %v", kb)
	}
	for _, row := range rows {
		for _, btn := range row {
			if btn["text"] == "✍️ Type an answer" {
				assertUnderLimit(t, "question, type-an-answer", btn["callback_data"])
				return
			}
		}
	}
	t.Error("a choice question with free_form off has no Type-an-answer button")
}

// A very long option is the worst case and must still fit, since the token is
// fixed width.
func TestRenderedQuestionKeyboardFitsWithAnEnormousOption(t *testing.T) {
	kb := questionKeyboard(pendingCallback{SessionID: "tg-test", RequestID: realisticCallID},
		[]string{strings.Repeat("a very long answer option ", 12)}, true)
	assertKeyboardUnderLimit(t, "question, huge option", kb)
}

func assertKeyboardUnderLimit(t *testing.T, what string, kb map[string]any) {
	t.Helper()
	rows, ok := kb["inline_keyboard"].([][]map[string]string)
	if !ok {
		t.Fatalf("%s keyboard has no rows: %v", what, kb)
	}
	if len(rows) == 0 {
		t.Fatalf("%s keyboard is empty", what)
	}
	for _, row := range rows {
		for _, btn := range row {
			assertUnderLimit(t, what, btn["callback_data"])
		}
	}
}

// The token has to carry the whole answer, so a long option is still answered
// correctly after it stops being in the callback data.
func TestQuestionTokenCarriesTheAnswer(t *testing.T) {
	const answer = "Run `git read-tree HEAD` to rebuild the index, then continue."
	ref := newCallbackToken(pendingCallback{SessionID: "tg-test", RequestID: "req-1", Answer: answer})
	got, ok := takeCallbackToken(ref)
	if !ok {
		t.Fatal("a fresh token did not resolve")
	}
	if got.Answer != answer {
		t.Errorf("answer = %q, want %q", got.Answer, answer)
	}
	if got.RequestID != "req-1" {
		t.Errorf("request id = %q, want req-1", got.RequestID)
	}
}

// One tap, one answer. A token left resolvable would let a second tap on an old
// message answer a request that has since been replaced.
func TestCallbackTokenIsSingleUse(t *testing.T) {
	ref := newCallbackToken(pendingCallback{SessionID: "tg-test", RequestID: "req-1", Answer: "yes"})
	if _, ok := takeCallbackToken(ref); !ok {
		t.Fatal("first take failed")
	}
	if _, ok := takeCallbackToken(ref); ok {
		t.Error("the token resolved twice; a replayed tap would answer a new request")
	}
}

// The permission path peeks rather than consumes, because the three buttons share
// one token and a rejected answer still has to be able to report honestly.
func TestCallbackTokenPeekDoesNotConsume(t *testing.T) {
	ref := newCallbackToken(pendingCallback{SessionID: "tg-test", RequestID: "req-1", Tool: "bash"})
	if _, ok := peekCallbackToken(ref); !ok {
		t.Fatal("peek failed")
	}
	if _, ok := peekCallbackToken(ref); !ok {
		t.Error("a second peek failed; peek must not consume")
	}
	if _, ok := takeCallbackToken(ref); !ok {
		t.Error("take after peek failed")
	}
}

func TestCallbackTokensExpire(t *testing.T) {
	ref := newCallbackToken(pendingCallback{SessionID: "tg-test", RequestID: "req-1", Answer: "yes"})

	// Age the entry past the TTL rather than sleeping for it.
	callbackTokenMu.Lock()
	callbackTokenSeen[ref] = time.Now().Add(-2 * callbackTokenTTL)
	callbackTokenMu.Unlock()

	sweepCallbackTokens(time.Now())
	if _, ok := peekCallbackToken(ref); ok {
		t.Error("an expired token still resolves; an unanswered prompt would leak forever")
	}
}

func TestCallbackTokenRejectsUnknownID(t *testing.T) {
	if _, ok := takeCallbackToken("deadbeefdeadbeef"); ok {
		t.Error("an unknown token resolved")
	}
	if _, ok := peekCallbackToken("deadbeefdeadbeef"); ok {
		t.Error("an unknown token resolved on peek")
	}
}

// The tokens have to be short, or the fix reintroduces the original problem at a
// smaller scale.
func TestCallbackTokenLength(t *testing.T) {
	ref := newCallbackToken(pendingCallback{RequestID: realisticCallID})
	if n := len(ref); n != 2*callbackTokenBytes {
		t.Errorf("token is %d chars, want %d", n, 2*callbackTokenBytes)
	}
	if strings.ContainsAny(ref, ":") {
		t.Error("a token contains the separator used by the callback grammar")
	}
}
