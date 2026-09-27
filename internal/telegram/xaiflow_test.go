package telegram

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/oauthdevice"
)

func captureXaiNotify(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	xaiNotify = func(token string, chatID int64, text string, kb map[string]any) {
		mu.Lock()
		seen = append(seen, text)
		mu.Unlock()
	}
	t.Cleanup(func() {
		xaiNotify = func(token string, chatID int64, text string, kb map[string]any) {
			sendHTML(token, chatID, text, kb)
		}
	})
	return &seen
}

// An untracked flow must be silent. This is the branch that decides whether to
// speak at all, so it must not be reachable only by inspection.
func TestXaiCompletionWithNoOwnerIsQuiet(t *testing.T) {
	seen := captureXaiNotify(t)

	deliverXaiTerminal("test-token", oauthdevice.Snapshot{ID: "untracked", Status: oauthdevice.StatusSucceeded})

	if len(*seen) != 0 {
		t.Errorf("an untracked flow still reported: %v", *seen)
	}
}

// A sign-in that did not ask to switch must say so.
func TestXaiCompletionWithoutActivateSaysItStayedInactive(t *testing.T) {
	seen := captureXaiNotify(t)

	trackXaiFlow("f1", 5, false)
	defer clearXaiFlow("f1")
	deliverXaiTerminal("test-token", oauthdevice.Snapshot{ID: "f1", Status: oauthdevice.StatusSucceeded})

	if len(*seen) != 1 {
		t.Fatalf("got %d notifications, want 1", len(*seen))
	}
	if !strings.Contains((*seen)[0], "not the active provider") {
		t.Errorf("a non-activating sign-in did not say it stayed inactive: %q", (*seen)[0])
	}
}

// A cancel is the user's own action and must not be reported as a failure.
func TestXaiCancelIsNotReportedTwice(t *testing.T) {
	seen := captureXaiNotify(t)

	trackXaiFlow("f-cancel", 12, true)
	defer clearXaiFlow("f-cancel")
	deliverXaiTerminal("test-token", oauthdevice.Snapshot{ID: "f-cancel", Status: oauthdevice.StatusCanceled})

	if len(*seen) != 0 {
		t.Errorf("a deliberate cancel reported: %v", *seen)
	}
	if _, ok := xaiFlowOwnerOf("f-cancel"); ok {
		t.Error("a handled cancel was not cleared, so it would report twice")
	}
}

// A completed login must reach the owning chat, which is what makes a
// menu-started login finish without a tap.
func TestXaiSuccessReachesTheOwningChat(t *testing.T) {
	seen := captureXaiNotify(t)

	trackXaiFlow("f-ok", 88, false)
	defer clearXaiFlow("f-ok")
	deliverXaiTerminal("test-token", oauthdevice.Snapshot{ID: "f-ok", Status: oauthdevice.StatusSucceeded})

	if len(*seen) != 1 {
		t.Fatalf("a completed login reported %d time(s), want 1", len(*seen))
	}
	if !strings.Contains((*seen)[0], "completed") {
		t.Errorf("the confirmation did not say it completed: %q", (*seen)[0])
	}
}

// Delivering twice must not report twice.
func TestXaiSuccessIsNotDeliveredTwice(t *testing.T) {
	seen := captureXaiNotify(t)

	trackXaiFlow("f-once", 88, false)
	defer clearXaiFlow("f-once")
	snapshot := oauthdevice.Snapshot{ID: "f-once", Status: oauthdevice.StatusSucceeded}
	deliverXaiTerminal("test-token", snapshot)
	deliverXaiTerminal("test-token", snapshot)

	if len(*seen) != 1 {
		t.Errorf("a single login reported %d time(s), want 1", len(*seen))
	}
}

// A failure must be explained and leave a way back in.
func TestXaiLoginFailureExplainsAndOffersRetry(t *testing.T) {
	seen := captureXaiNotify(t)

	trackXaiFlow("f-fail", 6, true)
	defer clearXaiFlow("f-fail")
	deliverXaiTerminal("test-token", oauthdevice.Snapshot{
		ID: "f-fail", Status: oauthdevice.StatusFailed, Message: "Authorization expired",
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

// The button existing and being handled are separate facts.
func TestProviderXaiCallbackReachesTheEntryPoint(t *testing.T) {
	var mu sync.Mutex
	called := 0
	original := xaiEntryPoint
	xaiEntryPoint = func(token string, chatID int64) {
		mu.Lock()
		called++
		mu.Unlock()
	}
	t.Cleanup(func() { xaiEntryPoint = original })

	var query callbackQuery
	if err := json.Unmarshal([]byte(`{"id":"1","data":"provider:xai","message":{"chat":{"id":4242},"message_id":5}}`), &query); err != nil {
		t.Fatalf("callback: %v", err)
	}
	handleCallback("test-token", &query)

	mu.Lock()
	defer mu.Unlock()
	if called != 1 {
		t.Errorf("provider:xai reached the entry point %d time(s); the button is dead", called)
	}
}

// The keyboard must route back to xAI, or the user is stranded.
func TestXaiStatusKeyboardAlwaysOffersAnAction(t *testing.T) {
	for _, signedIn := range []bool{false, true} {
		kb := xaiStatusKeyboard(signedIn)
		rows, ok := kb["inline_keyboard"].([][]map[string]string)
		if !ok || len(rows) == 0 {
			t.Fatalf("signedIn=%v produced no buttons", signedIn)
		}
		encoded, err := json.Marshal(kb)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), "provider:xai") {
			t.Errorf("signedIn=%v keyboard does not route back to xAI: %s", signedIn, encoded)
		}
	}
}

// The activation model must be one Grok serves, never carried over from whatever
// provider was active.
func TestXaiActivationModelIsNeverCarriedOverFromAnotherProvider(t *testing.T) {
	_, current := defaultState.values()
	if llm.IsXaiModel(current) {
		t.Skip("the active model is already an xAI model, so there is nothing to guard")
	}
	_, model := xaiModelForAccount("")
	if !llm.IsXaiModel(model) {
		t.Errorf("activation chose %q, which Grok does not serve; it was carried over from %q", model, current)
	}
}
