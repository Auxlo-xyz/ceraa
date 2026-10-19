package telegram

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"ceraa/harness/question"
)

// Telegram rejects a whole message whose inline_keyboard carries callback_data
// longer than 64 bytes: not the offending button, the message. So an over-long
// button does not degrade, it deletes the menu.
//
// Two menus were over the limit and both failed in production. The permission
// buttons inlined a provider call id, already 41 bytes, plus a decision and a tool
// name: 71 to 78 bytes. The question buttons inlined the same call id plus the
// answer text, which is model prose: 109 bytes for one ordinary sentence. A
// permission prompt that cannot be displayed is worse than no prompt, because the
// tool then blocks for its full five minute timeout, retries, and the run looks
// like a stalled model.
//
// So nothing unbounded goes in callback_data. Each pending request gets a short
// random token, and the token carries a fixed-width reference to state held here.
// The request id, the tool name and the answer text stay on this side.

// callbackTokenBytes is 8, so a token is 16 hex characters. Comfortably inside the
// limit even with a prefix and a decision code, and short enough to stay readable
// in a callback log.
const callbackTokenBytes = 8

// pendingCallback is what a token stands for.
type pendingCallback struct {
	SessionID string
	RequestID string
	ChatID    int64
	Tool      string
	// Answer is the text a question button submits. Empty for a permission, which
	// carries its decision in the callback data instead.
	Answer string
}

var (
	callbackTokenMu sync.Mutex
	callbackTokens  = map[string]pendingCallback{}
	// callbackTokenSeen stores each token's expiry rather than its lit time, because
	// one token does not have one lifetime: a permission button is stale after
	// ten minutes, while a question button has to outlive the hour-long question
	// it belongs to.
	callbackTokenSeen = make(map[string]time.Time)
)

// callbackTokenTTL is the lifetime of a token the caller does not specifically
// bound: the permission prompt, whose answer window is five minutes anyway.
// Question tokens mint with a longer one from questionKeyboard.
const callbackTokenTTL = 10 * time.Minute

// newCallbackToken mints a token and returns it alongside the id it is stored
// under, so the caller has only one value to thread through.
func newCallbackToken(p pendingCallback) string {
	return newCallbackTokenWithTTL(p, callbackTokenTTL)
}

// newCallbackTokenWithTTL mints a token that lives for ttl. The caller picks the
// lifetime because the map sweep is the only thing that retires a token, and a
// question's buttons must survive at least as long as the question does.
func newCallbackTokenWithTTL(p pendingCallback, ttl time.Duration) string {
	buf := make([]byte, callbackTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is not a condition to ship a colliding token.
		panic("ceraa: crypto/rand unavailable: " + err.Error())
	}
	id := hex.EncodeToString(buf)
	callbackTokenMu.Lock()
	defer callbackTokenMu.Unlock()
	sweepCallbackTokensLocked(time.Now())
	callbackTokens[id] = p
	callbackTokenSeen[id] = time.Now().Add(ttl)
	return id
}

// takeCallbackToken resolves a token, consuming it. One tap, one answer: leaving
// it in the map would let a second tap on an old message answer a new request.
func takeCallbackToken(id string) (pendingCallback, bool) {
	callbackTokenMu.Lock()
	defer callbackTokenMu.Unlock()
	p, ok := callbackTokens[id]
	if !ok {
		return pendingCallback{}, false
	}
	delete(callbackTokens, id)
	delete(callbackTokenSeen, id)
	return p, true
}

// peekCallbackToken resolves without consuming, for the permission path where the
// decision code is carried alongside the token and the same token legitimately
// arrives once per button rather than once per answer.
func peekCallbackToken(id string) (pendingCallback, bool) {
	callbackTokenMu.Lock()
	defer callbackTokenMu.Unlock()
	p, ok := callbackTokens[id]
	return p, ok
}

func dropCallbackToken(id string) {
	callbackTokenMu.Lock()
	defer callbackTokenMu.Unlock()
	delete(callbackTokens, id)
	delete(callbackTokenSeen, id)
}

// sweepCallbackTokensLocked drops expired entries, visiting a bounded number per
// call so a burst of renders cannot turn into a long pause.
func sweepCallbackTokens(now time.Time) {
	callbackTokenMu.Lock()
	defer callbackTokenMu.Unlock()
	sweepCallbackTokensLocked(now)
}

func sweepCallbackTokensLocked(now time.Time) {
	checked := 0
	for id, expires := range callbackTokenSeen {
		if now.After(expires) {
			delete(callbackTokens, id)
			delete(callbackTokenSeen, id)
		}
		checked++
		if checked >= 64 {
			return
		}
	}
}

// permissionKeyboard builds the permission prompt's keyboard.
//
// One token, three buttons. The request id, the tool name and the decision words
// used to be inlined here, which put callback_data at 71 to 78 bytes against a 64
// byte limit, and Telegram rejects the whole message rather than the one button.
// The prompt then never appeared, and the tool sat on its five minute timeout
// with nothing on screen to answer it — which reads as a stalled model.
func permissionKeyboard(p pendingCallback) map[string]any {
	ref := newCallbackToken(p)
	return map[string]any{"inline_keyboard": [][]map[string]string{{
		{"text": "✓ Allow once", "callback_data": "perm:" + ref + ":a"},
		{"text": "✓ Always", "callback_data": "perm:" + ref + ":s"},
		{"text": "✕ Deny", "callback_data": "perm:" + ref + ":d"},
	}}}
}

// questionKeyboard builds a question's keyboard.
//
// A token per option, so the answer text never enters callback_data. It used to be
// inlined whole: one ordinary sentence put the data at 109 bytes, so a question
// with real options in it was rejected outright and could not be rendered at all.
//
// Every token here outlives the question itself: the manager waits an hour
// (question.DefaultTimeout), so a button that dies at the generic ten-minute
// sweep answers taps with "expired" while the card is still the live question.
func questionKeyboard(p pendingCallback, options []string, freeForm bool) map[string]any {
	var rows [][]map[string]string
	newQuestionToken := func(p pendingCallback) string {
		return newCallbackTokenWithTTL(p, question.DefaultTimeout+5*time.Minute)
	}
	for _, opt := range options {
		optRef := newQuestionToken(pendingCallback{SessionID: p.SessionID, RequestID: p.RequestID, ChatID: p.ChatID, Answer: opt})
		rows = append(rows, []map[string]string{{"text": opt, "callback_data": "qans:" + optRef}})
	}
	// The type-an-answer row is unconditional. Without a way to type, the user
	// is limited to the exact strings offered, which is wrong whenever the
	// answer is a value, a path, or a name — and offering options never means
	// the answer must be one of them. freeForm still matters to the renderer:
	// it marks the next plain message as an answer up front, while here the
	// user opts into typing by tapping first.
	typeRef := newQuestionToken(pendingCallback{SessionID: p.SessionID, RequestID: p.RequestID, ChatID: p.ChatID})
	rows = append(rows, []map[string]string{{"text": "✍️ Type an answer", "callback_data": "qtype:" + typeRef}})
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}
