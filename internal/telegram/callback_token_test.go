package telegram

import (
	"strings"
	"testing"
	"time"
)

// questionRefsFromKeyboard pulls one qans token out of a built keyboard, so the
// TTL tests assert on the same bytes a tap would carry.
func questionRefsFromKeyboard(t *testing.T, kb map[string]any, prefix string) []string {
	t.Helper()
	rows, ok := kb["inline_keyboard"].([][]map[string]string)
	if !ok {
		t.Fatal("keyboard is not an inline_keyboard")
	}
	var refs []string
	for _, row := range rows {
		for _, button := range row {
			if ref, found := strings.CutPrefix(button["callback_data"], prefix); found {
				refs = append(refs, ref)
			}
		}
	}
	return refs
}

// A question waits an hour (question.DefaultTimeout), so its buttons must wait
// at least that long. The old sweep retired every token at ten minutes, which
// answered a fourteen minute tap on a still-waiting question with "expired.
// Ask again." while the card was the live question.
func TestQuestionButtonsOutliveTheTenMinuteSweep(t *testing.T) {
	const session = "ttl-q"
	kb := questionKeyboard(pendingCallback{SessionID: session, RequestID: "r", ChatID: 7}, []string{"opt one", "opt two"}, false)
	refs := questionRefsFromKeyboard(t, kb, "qans:")
	if len(refs) == 0 {
		t.Fatal("no option buttons were minted")
	}
	var typeRef string
	if qtype := questionRefsFromKeyboard(t, kb, "qtype:"); len(qtype) == 1 {
		typeRef = qtype[0]
	} else {
		t.Fatalf("minted %d typed-answer buttons, want 1", len(qtype))
	}

	// Eleven minutes in, every token the old sweep would have killed is gone;
	// the question buttons are still live.
	sweepCallbackTokens(time.Now().Add(11 * time.Minute))
	for _, ref := range refs {
		if _, ok := takeCallbackToken(ref); !ok {
			t.Error("a question button was dead eleven minutes in, while the question waits an hour")
		}
	}
	if _, ok := takeCallbackToken(typeRef); !ok {
		t.Error("the typed-answer button was dead eleven minutes in")
	}
	if got := tokensForSession(session); got != 0 {
		t.Errorf("%d tokens of the session are left after taking every button", got)
	}

	// And the buttons do end: past the question's hour the sweep retires them.
	kb2 := questionKeyboard(pendingCallback{SessionID: session, RequestID: "r2", ChatID: 7}, []string{"opt one"}, false)
	late := []string{questionRefsFromKeyboard(t, kb2, "qans:")[0]}
	late = append(late, questionRefsFromKeyboard(t, kb2, "qtype:")...)
	sweepCallbackTokens(time.Now().Add(2 * time.Hour))
	for _, ref := range late {
		if _, ok := takeCallbackToken(ref); ok {
			t.Error("a question token survived past the question's hour")
		}
	}
}

// The default window still applies to what it was written for: a permission
// button is stale after ten minutes and must not answer on a fourteen minute tap.
func TestPermissionButtonsFollowTheirOwnWindow(t *testing.T) {
	const session = "ttl-perm"
	ref := newCallbackToken(pendingCallback{SessionID: session, RequestID: "r", ChatID: 7})
	sweepCallbackTokens(time.Now().Add(11 * time.Minute))
	if _, ok := takeCallbackToken(ref); ok {
		t.Error("a permission token outlived its ten-minute window")
	}
	if got := tokensForSession(session); got != 0 {
		t.Errorf("%d tokens of the session are left", got)
	}
}

// tokensForSession counts this session's entries, so a shared test binary's
// other tokens never sharpen the assertion.
func tokensForSession(session string) int {
	callbackTokenMu.Lock()
	defer callbackTokenMu.Unlock()
	count := 0
	for _, p := range callbackTokens {
		if p.SessionID == session {
			count++
		}
	}
	return count
}
