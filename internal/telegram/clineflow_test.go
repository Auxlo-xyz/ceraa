package telegram

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/oauthdevice"
)

func captureClineNotify(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	clineNotify = func(token string, chatID int64, text string, kb map[string]any) {
		mu.Lock()
		seen = append(seen, text)
		mu.Unlock()
	}
	t.Cleanup(func() {
		clineNotify = func(token string, chatID int64, text string, kb map[string]any) {
			sendHTML(token, chatID, text, kb)
		}
	})
	return &seen
}

func TestClineCompletionWithNoOwnerIsQuiet(t *testing.T) {
	seen := captureClineNotify(t)
	deliverClineTerminal("test-token", oauthdevice.Snapshot{ID: "untracked", Status: oauthdevice.StatusSucceeded})
	if len(*seen) != 0 {
		t.Errorf("an untracked flow still reported: %v", *seen)
	}
}

func TestClineCompletionWithoutActivateSaysItStayedInactive(t *testing.T) {
	seen := captureClineNotify(t)
	trackClineFlow("f1", 5, false)
	defer clearClineFlow("f1")
	deliverClineTerminal("test-token", oauthdevice.Snapshot{ID: "f1", Status: oauthdevice.StatusSucceeded})
	if len(*seen) != 1 {
		t.Fatalf("got %d notifications, want 1", len(*seen))
	}
	if !strings.Contains((*seen)[0], "not the active provider") {
		t.Errorf("a non-activating sign-in did not say it stayed inactive: %q", (*seen)[0])
	}
}

func TestClineCancelIsNotReportedTwice(t *testing.T) {
	seen := captureClineNotify(t)
	trackClineFlow("f-cancel", 12, true)
	defer clearClineFlow("f-cancel")
	deliverClineTerminal("test-token", oauthdevice.Snapshot{ID: "f-cancel", Status: oauthdevice.StatusCanceled})
	if len(*seen) != 0 {
		t.Errorf("a deliberate cancel reported: %v", *seen)
	}
	if _, ok := clineFlowOwnerOf("f-cancel"); ok {
		t.Error("a handled cancel was not cleared, so it would report twice")
	}
}

func TestClineSuccessReachesTheOwningChat(t *testing.T) {
	seen := captureClineNotify(t)
	trackClineFlow("f-ok", 88, false)
	defer clearClineFlow("f-ok")
	deliverClineTerminal("test-token", oauthdevice.Snapshot{ID: "f-ok", Status: oauthdevice.StatusSucceeded})
	if len(*seen) != 1 {
		t.Fatalf("a completed login reported %d time(s), want 1", len(*seen))
	}
}

func TestClineSuccessIsNotDeliveredTwice(t *testing.T) {
	seen := captureClineNotify(t)
	trackClineFlow("f-once", 88, false)
	defer clearClineFlow("f-once")
	snapshot := oauthdevice.Snapshot{ID: "f-once", Status: oauthdevice.StatusSucceeded}
	deliverClineTerminal("test-token", snapshot)
	deliverClineTerminal("test-token", snapshot)
	if len(*seen) != 1 {
		t.Errorf("a single login reported %d time(s), want 1", len(*seen))
	}
}

func TestClineLoginFailureExplainsAndOffersRetry(t *testing.T) {
	seen := captureClineNotify(t)
	trackClineFlow("f-fail", 6, true)
	defer clearClineFlow("f-fail")
	deliverClineTerminal("test-token", oauthdevice.Snapshot{
		ID: "f-fail", Status: oauthdevice.StatusFailed, Message: "Authorization expired",
	})
	if len(*seen) != 1 {
		t.Fatalf("got %d notifications, want 1", len(*seen))
	}
	if !strings.Contains((*seen)[0], "Authorization expired") {
		t.Errorf("the failure reason was dropped: %q", (*seen)[0])
	}
}

func TestProviderClineCallbackReachesTheEntryPoint(t *testing.T) {
	var mu sync.Mutex
	called := 0
	original := clineEntryPoint
	clineEntryPoint = func(token string, chatID int64) {
		mu.Lock()
		called++
		mu.Unlock()
	}
	t.Cleanup(func() { clineEntryPoint = original })

	var query callbackQuery
	if err := json.Unmarshal([]byte(`{"id":"1","data":"provider:cline","message":{"chat":{"id":6161},"message_id":5}}`), &query); err != nil {
		t.Fatalf("callback: %v", err)
	}
	handleCallback("test-token", &query)

	mu.Lock()
	defer mu.Unlock()
	if called != 1 {
		t.Errorf("provider:cline reached the entry point %d time(s); the button is dead", called)
	}
}

func TestClineStatusKeyboardAlwaysOffersAnAction(t *testing.T) {
	for _, signedIn := range []bool{false, true} {
		kb := clineStatusKeyboard(signedIn)
		rows, ok := kb["inline_keyboard"].([][]map[string]string)
		if !ok || len(rows) == 0 {
			t.Fatalf("signedIn=%v produced no buttons", signedIn)
		}
		encoded, err := json.Marshal(kb)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), "provider:cline") {
			t.Errorf("signedIn=%v keyboard does not route back to Cline: %s", signedIn, encoded)
		}
	}
}

func TestClineActivationModelIsNeverCarriedOver(t *testing.T) {
	_, current := defaultState.values()
	if llm.IsClineModel(current) {
		t.Skip("the active model is already a Cline model, so there is nothing to guard")
	}
	model := clineModelForAccount("")
	if !llm.IsClineModel(model) {
		t.Errorf("activation chose %q, which the gateway does not serve; carried over from %q", model, current)
	}
}

// The default has to be on the free tier, since that is what a signed-in account
// can actually use.
func TestDefaultClineModelIsOnTheFreeTier(t *testing.T) {
	if !llm.IsClineModel(llm.DefaultClineModel) {
		t.Errorf("default model %q is not in the free tier", llm.DefaultClineModel)
	}
	if !strings.HasPrefix(llm.ClineGatewayBase, "https://api.cline.bot") {
		t.Errorf("gateway base = %q", llm.ClineGatewayBase)
	}
}
