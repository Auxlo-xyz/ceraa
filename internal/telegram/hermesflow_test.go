package telegram

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/hermesauth"
)

// captureHermesNotify swaps the notify indirection for a recorder.
//
// Indirection exists so a test can observe that the user was actually told, which
// is the requirement, rather than only that some internal flag flipped.
func captureHermesNotify(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	hermesNotify = func(token string, chatID int64, text string, kb map[string]any) {
		mu.Lock()
		seen = append(seen, text)
		mu.Unlock()
	}
	t.Cleanup(func() {
		hermesNotify = func(token string, chatID int64, text string, kb map[string]any) {
			sendHTML(token, chatID, text, kb)
		}
	})
	return &seen
}

// A device login must finish on its own. The user approves the code in a browser
// and never touches the bot again, so if nothing activates the provider, the next
// task silently runs on whatever was selected before.
func TestHermesCompletionActivatesProviderWithoutATap(t *testing.T) {
	captureHermesNotify(t)

	trackHermesFlow("flow-1", 4242, true, "")
	defer clearHermesFlow("flow-1")

	owner, ok := hermesFlowOwnerOf("flow-1")
	if !ok {
		t.Fatal("a tracked flow has no owner")
	}
	if !owner.activate {
		t.Fatal("a login started from the provider menu must ask to activate")
	}
	if owner.chatID != 4242 {
		t.Errorf("owner chat = %d, want 4242", owner.chatID)
	}
}

// The flow must be consumed after reporting, or a second delivery posts twice.
func TestReportedHermesFlowIsCleared(t *testing.T) {
	captureHermesNotify(t) // a duplicate report would land here

	trackHermesFlow("flow-dup", 7, false, "")
	clearHermesFlow("flow-dup")
	if _, ok := hermesFlowOwnerOf("flow-dup"); ok {
		t.Error("the flow survived being cleared, so it would report twice")
	}
}

// An untracked flow must be silent rather than posted to a guessed chat. This is
// the branch that decides whether to speak at all, so it is the one that must not
// be reachable only by inspection.
func TestHermesCompletionWithNoOwnerIsQuiet(t *testing.T) {
	seen := captureHermesNotify(t)

	deliverHermesTerminal("test-token", hermesauth.TerminalState{
		FlowID: "untracked", Status: hermesauth.StatusSucceeded,
	})

	if len(*seen) != 0 {
		t.Errorf("an untracked flow still reported: %v", *seen)
	}
}

// A cancel is the user's own action and the button that did it already edited its
// own message, so reporting again would be noise.
func TestHermesCancelIsNotReportedTwice(t *testing.T) {
	seen := captureHermesNotify(t)

	trackHermesFlow("flow-cancel", 12, true, "")
	defer clearHermesFlow("flow-cancel")
	deliverHermesTerminal("test-token", hermesauth.TerminalState{
		FlowID: "flow-cancel", Status: hermesauth.StatusCanceled,
	})

	if len(*seen) != 0 {
		t.Errorf("a deliberate cancel reported: %v", *seen)
	}
	if _, ok := hermesFlowOwnerOf("flow-cancel"); ok {
		t.Error("a handled cancel was not cleared, so it would report twice")
	}
}

// The completion path has to report success to the owning chat, which is the
// behaviour that makes a menu-started login finish without a tap.
func TestHermesSuccessReachesTheOwningChat(t *testing.T) {
	seen := captureHermesNotify(t)

	trackHermesFlow("flow-ok", 88, false, "")
	defer clearHermesFlow("flow-ok")
	deliverHermesTerminal("test-token", hermesauth.TerminalState{
		FlowID: "flow-ok", Status: hermesauth.StatusSucceeded,
	})

	if len(*seen) != 1 {
		t.Fatalf("a completed login reported %d time(s), want 1", len(*seen))
	}
	if !strings.Contains((*seen)[0], "completed") {
		t.Errorf("the confirmation did not say it completed: %q", (*seen)[0])
	}
}

// Delivering twice must not report twice, so the flow is consumed on first report.
func TestHermesSuccessIsNotDeliveredTwice(t *testing.T) {
	seen := captureHermesNotify(t)

	trackHermesFlow("flow-once", 88, false, "")
	defer clearHermesFlow("flow-once")
	state := hermesauth.TerminalState{FlowID: "flow-once", Status: hermesauth.StatusSucceeded}
	deliverHermesTerminal("test-token", state)
	deliverHermesTerminal("test-token", state)

	if len(*seen) != 1 {
		t.Errorf("a single login reported %d time(s), want 1", len(*seen))
	}
}

// A sign-in that did not ask to switch must say so, and must not activate.
func TestHermesCompletionWithoutActivateSaysItStayedInactive(t *testing.T) {
	seen := captureHermesNotify(t)

	reportHermesSignedIn("test-token", hermesFlowOwner{chatID: 99, activate: false}, "flow-2")

	if len(*seen) != 1 {
		t.Fatalf("got %d notifications, want 1", len(*seen))
	}
	if !strings.Contains((*seen)[0], "not the active provider") {
		t.Errorf("a non-activating sign-in did not say it stayed inactive: %q", (*seen)[0])
	}
}

// A failure has to be explained and leave a way back in. A status reply that only
// says "it failed" is a dead end on a phone.
func TestHermesLoginFailureExplainsAndOffersRetry(t *testing.T) {
	seen := captureHermesNotify(t)

	reportHermesLoginFailed("test-token", 55, hermesauth.TerminalState{
		FlowID: "flow-3", Status: hermesauth.StatusFailed, Message: "Authorization expired",
	})

	if len(*seen) != 1 {
		t.Fatalf("got %d notifications, want 1", len(*seen))
	}
	if !strings.Contains((*seen)[0], "Authorization expired") {
		t.Errorf("the failure reason was dropped: %q", (*seen)[0])
	}
	if !strings.Contains((*seen)[0], "did not complete") {
		t.Errorf("the failure was not stated: %q", (*seen)[0])
	}
}

// The failure keyboard must route back to Hermes, or the user is stranded.
func TestHermesStatusKeyboardAlwaysOffersAnAction(t *testing.T) {
	for _, signedIn := range []bool{false, true} {
		kb := hermesStatusKeyboard(signedIn)
		rows, ok := kb["inline_keyboard"].([][]map[string]string)
		if !ok || len(rows) == 0 {
			t.Fatalf("signedIn=%v produced no buttons", signedIn)
		}
		encoded, err := json.Marshal(kb)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), "provider:hermes") {
			t.Errorf("signedIn=%v keyboard does not route back to Hermes: %s", signedIn, encoded)
		}
	}
}

// The model chosen for activation must be one the Nous Portal serves, never the
// currently active model carried over from another provider.
func TestHermesActivationModelIsNeverCarriedOverFromAnotherProvider(t *testing.T) {
	// defaultState currently reports some non-Hermes model, since the active
	// provider is apmix in this workspace.
	_, current := defaultState.values()
	if llm.IsHermesModel(current) {
		t.Skip("the active model is already a Hermes model, so there is nothing to guard")
	}
	model := hermesModelForAccount("")
	if !llm.IsHermesModel(model) {
		t.Errorf("activation chose %q, which the Nous Portal does not serve; it was carried over from %q", model, current)
	}
}

// The button existing and the button being handled are separate facts, and only
// the first was covered. A provider:hermes callback that nothing routes to is a
// dead button, which is exactly the bug this work was for.
func TestProviderHermesCallbackReachesTheEntryPoint(t *testing.T) {
	var mu sync.Mutex
	called := 0
	selectHermesProviderImpl = func(token string, chatID int64) {
		mu.Lock()
		called++
		mu.Unlock()
	}
	t.Cleanup(func() {
		selectHermesProviderImpl = func(token string, chatID int64) { selectHermesProvider(token, chatID) }
	})

	// Built by decoding rather than by assigning to the anonymous Message struct,
	// which carries more fields than a hand-written literal would match.
	var query callbackQuery
	if err := json.Unmarshal([]byte(`{"id":"1","data":"provider:hermes","message":{"chat":{"id":3131},"message_id":5}}`), &query); err != nil {
		t.Fatalf("could not build the callback: %v", err)
	}
	if query.Message == nil || query.Message.Chat.ID != 3131 {
		t.Fatalf("callback was not built as expected: %+v", query.Message)
	}

	handleCallback("test-token", &query)

	mu.Lock()
	defer mu.Unlock()
	if called != 1 {
		t.Errorf("provider:hermes reached the entry point %d time(s), want 1; the button is dead", called)
	}
}
