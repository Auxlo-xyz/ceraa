package telegram

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/oauthdevice"
)

func captureMinimaxNotify(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	minimaxNotify = func(token string, chatID int64, text string, kb map[string]any) {
		mu.Lock()
		seen = append(seen, text)
		mu.Unlock()
	}
	t.Cleanup(func() {
		minimaxNotify = func(token string, chatID int64, text string, kb map[string]any) {
			sendHTML(token, chatID, text, kb)
		}
	})
	return &seen
}

// An untracked flow must be silent. This is the branch that decides whether to
// speak at all, so it must not be reachable only by inspection.
func TestMinimaxCompletionWithNoOwnerIsQuiet(t *testing.T) {
	seen := captureMinimaxNotify(t)
	deliverMinimaxTerminal("test-token", oauthdevice.Snapshot{ID: "untracked", Status: oauthdevice.StatusSucceeded})
	if len(*seen) != 0 {
		t.Errorf("an untracked flow still reported: %v", *seen)
	}
}

func TestMinimaxCompletionWithoutActivateSaysItStayedInactive(t *testing.T) {
	seen := captureMinimaxNotify(t)
	trackMinimaxFlow("f1", 5, false, "")
	defer clearMinimaxFlow("f1")
	deliverMinimaxTerminal("test-token", oauthdevice.Snapshot{ID: "f1", Status: oauthdevice.StatusSucceeded})
	if len(*seen) != 1 {
		t.Fatalf("got %d notifications, want 1", len(*seen))
	}
	if !strings.Contains((*seen)[0], "not the active provider") {
		t.Errorf("a non-activating sign-in did not say it stayed inactive: %q", (*seen)[0])
	}
}

// A cancel is the user's own action and must not be reported as a failure.
func TestMinimaxCancelIsNotReportedTwice(t *testing.T) {
	seen := captureMinimaxNotify(t)
	trackMinimaxFlow("f-cancel", 12, true, "")
	defer clearMinimaxFlow("f-cancel")
	deliverMinimaxTerminal("test-token", oauthdevice.Snapshot{ID: "f-cancel", Status: oauthdevice.StatusCanceled})
	if len(*seen) != 0 {
		t.Errorf("a deliberate cancel reported: %v", *seen)
	}
	if _, ok := minimaxFlowOwnerOf("f-cancel"); ok {
		t.Error("a handled cancel was not cleared, so it would report twice")
	}
}

func TestMinimaxSuccessReachesTheOwningChat(t *testing.T) {
	seen := captureMinimaxNotify(t)
	trackMinimaxFlow("f-ok", 88, false, "")
	defer clearMinimaxFlow("f-ok")
	deliverMinimaxTerminal("test-token", oauthdevice.Snapshot{ID: "f-ok", Status: oauthdevice.StatusSucceeded})
	if len(*seen) != 1 {
		t.Fatalf("a completed login reported %d time(s), want 1", len(*seen))
	}
}

func TestMinimaxSuccessIsNotDeliveredTwice(t *testing.T) {
	seen := captureMinimaxNotify(t)
	trackMinimaxFlow("f-once", 88, false, "")
	defer clearMinimaxFlow("f-once")
	snapshot := oauthdevice.Snapshot{ID: "f-once", Status: oauthdevice.StatusSucceeded}
	deliverMinimaxTerminal("test-token", snapshot)
	deliverMinimaxTerminal("test-token", snapshot)
	if len(*seen) != 1 {
		t.Errorf("a single login reported %d time(s), want 1", len(*seen))
	}
}

func TestMinimaxLoginFailureExplainsAndOffersRetry(t *testing.T) {
	seen := captureMinimaxNotify(t)
	trackMinimaxFlow("f-fail", 6, true, "")
	defer clearMinimaxFlow("f-fail")
	deliverMinimaxTerminal("test-token", oauthdevice.Snapshot{
		ID: "f-fail", Status: oauthdevice.StatusFailed, Message: "Authorization expired",
	})
	if len(*seen) != 1 {
		t.Fatalf("got %d notifications, want 1", len(*seen))
	}
	if !strings.Contains((*seen)[0], "Authorization expired") {
		t.Errorf("the failure reason was dropped: %q", (*seen)[0])
	}
}

// The button existing and being handled are separate facts.
func TestProviderMinimaxCallbackReachesTheEntryPoint(t *testing.T) {
	var mu sync.Mutex
	called := 0
	original := minimaxEntryPoint
	minimaxEntryPoint = func(token string, chatID int64) {
		mu.Lock()
		called++
		mu.Unlock()
	}
	t.Cleanup(func() { minimaxEntryPoint = original })

	var query callbackQuery
	if err := json.Unmarshal([]byte(`{"id":"1","data":"provider:minimax","message":{"chat":{"id":5151},"message_id":5}}`), &query); err != nil {
		t.Fatalf("callback: %v", err)
	}
	handleCallback("test-token", &query)

	mu.Lock()
	defer mu.Unlock()
	if called != 1 {
		t.Errorf("provider:minimax reached the entry point %d time(s); the button is dead", called)
	}
}

func TestMinimaxStatusKeyboardAlwaysOffersAnAction(t *testing.T) {
	for _, signedIn := range []bool{false, true} {
		kb := minimaxStatusKeyboard(signedIn)
		rows, ok := kb["inline_keyboard"].([][]map[string]string)
		if !ok || len(rows) == 0 {
			t.Fatalf("signedIn=%v produced no buttons", signedIn)
		}
		encoded, err := json.Marshal(kb)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), "provider:minimax") {
			t.Errorf("signedIn=%v keyboard does not route back to MiniMax: %s", signedIn, encoded)
		}
	}
}

// The activation model must be one MiniMax serves, never carried over from another
// provider.
func TestMinimaxActivationModelIsNeverCarriedOver(t *testing.T) {
	_, current := defaultState.values()
	if llm.IsMiniMaxModel(current) {
		t.Skip("the active model is already a MiniMax model, so there is nothing to guard")
	}
	model := minimaxModelForAccount("")
	if !llm.IsMiniMaxModel(model) {
		t.Errorf("activation chose %q, which the subscription does not serve; carried over from %q", model, current)
	}
}

// The default must be the model the agent backend actually serves, reached by a
// URL that resolves to the Messages endpoint.
//
// Both halves have moved and neither is obvious. The base is the agent backend
// rather than the platform API's /anthropic path, because the mcode grant issues
// an agent token and only the agent backend accepts one. And the base already ends
// in the API version, so the transport appends /v1/messages to it rather than
// substituting for the version it already carries.
func TestDefaultMiniMaxModelIsOnTheAgentBackendMessagesPath(t *testing.T) {
	if llm.DefaultMiniMaxModel != "MiniMax-M3" {
		t.Errorf("default model = %q", llm.DefaultMiniMaxModel)
	}
	if !strings.HasPrefix(llm.MiniMaxBaseURL, "https://agent.minimax.io/") {
		t.Errorf("base URL = %q; the agent token is refused by the platform API with a "+
			"401 asking for an API secret key in X-Api-Key", llm.MiniMaxBaseURL)
	}
	if llm.MiniMaxMessagesAuth != llm.MessagesAuthBearer {
		t.Errorf("messages auth = %q, want bearer; the agent backend answers x-api-key "+
			"with {\"token is required\"}", llm.MiniMaxMessagesAuth)
	}
}
