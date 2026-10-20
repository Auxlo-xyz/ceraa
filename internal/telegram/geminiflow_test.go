package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/geminiauth"
	"ceraa/internal/telegram/menus"
)

type geminiNotifications struct {
	mu   sync.Mutex
	seen []string
}

func (n *geminiNotifications) snapshot() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.seen...)
}

func captureGeminiNotify(t *testing.T) *geminiNotifications {
	t.Helper()
	seen := &geminiNotifications{}
	original := geminiNotify
	geminiNotify = func(token string, chatID int64, text string, kb map[string]any) {
		seen.mu.Lock()
		seen.seen = append(seen.seen, text)
		seen.mu.Unlock()
	}
	t.Cleanup(func() { geminiNotify = original })
	return seen
}

// The button existing and being handled are separate facts.
func TestProviderGeminiCallbackReachesTheEntryPoint(t *testing.T) {
	var mu sync.Mutex
	called := 0
	original := geminiEntryPoint
	geminiEntryPoint = func(token string, chatID int64) {
		mu.Lock()
		called++
		mu.Unlock()
	}
	t.Cleanup(func() { geminiEntryPoint = original })

	var query callbackQuery
	if err := json.Unmarshal([]byte(`{"id":"1","data":"provider:gemini","message":{"chat":{"id":5151},"message_id":5}}`), &query); err != nil {
		t.Fatalf("callback: %v", err)
	}
	handleCallback("test-token", &query)

	mu.Lock()
	defer mu.Unlock()
	if called != 1 {
		t.Errorf("provider:gemini reached the entry point %d time(s); the button is dead", called)
	}
}

// "Sign in again" must start a fresh login, not silently do nothing.
func TestGeminiReloginStartsALogin(t *testing.T) {
	seen := captureGeminiNotify(t)

	var query callbackQuery
	if err := json.Unmarshal([]byte(`{"id":"1","data":"provider:gemini:relogin","message":{"chat":{"id":5151},"message_id":5}}`), &query); err != nil {
		t.Fatalf("callback: %v", err)
	}
	handleCallback("test-token", &query)

	joined := strings.Join(seen.snapshot(), "\n")
	if !strings.Contains(joined, "accounts.google.com") {
		t.Errorf("provider:gemini:relogin sent no login instructions; notified:\n%v", seen.snapshot())
	}
}

// A pasted callback URL exchanges and activates when the switch is wanted.
func TestGeminiCodeStageActivates(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureGeminiNotify(t)
	if err := providers.setActive("opencode"); err != nil {
		t.Fatal(err)
	}

	original := geminiExchange
	geminiExchange = func(ctx context.Context, flow *geminiauth.Flow, code string) error { return nil }
	t.Cleanup(func() { geminiExchange = original })
	stubGeminiAccountCheck(t, nil, nil)

	trackGeminiFlow(5151, geminiFlowOwner{chatID: 5151, activate: true, activeAtStart: "opencode", flow: &geminiauth.Flow{}})
	pendingProvider.Store(5151, providerDraft{ID: "gemini", Stage: "code"})
	if !handleGeminiCodeStage("test-token", 5151, "http://localhost:8845/oauth-callback?code=abc&state=s") {
		t.Fatal("code stage declined the paste")
	}
	if got := providers.active(); got != "gemini" {
		t.Errorf("active = %q after a successful paste; want gemini", got)
	}
	if joined := strings.Join(seen.snapshot(), "\n"); !strings.Contains(joined, "Gemini provider activated") {
		t.Errorf("no activation confirmation; notified:\n%v", seen.snapshot())
	}
}

// A failed exchange tells the chat and saves nothing.
func TestGeminiCodeStageFailureNotifies(t *testing.T) {
	seen := captureGeminiNotify(t)

	original := geminiExchange
	geminiExchange = func(ctx context.Context, flow *geminiauth.Flow, code string) error {
		return context.DeadlineExceeded
	}
	t.Cleanup(func() { geminiExchange = original })

	trackGeminiFlow(5151, geminiFlowOwner{chatID: 5151, activate: true, activeAtStart: "opencode", flow: &geminiauth.Flow{}})
	pendingProvider.Store(5151, providerDraft{ID: "gemini", Stage: "code"})
	handleGeminiCodeStage("test-token", 5151, "code-abc")
	if joined := strings.Join(seen.snapshot(), "\n"); !strings.Contains(joined, "did not complete") {
		t.Errorf("no failure report; notified:\n%v", seen.snapshot())
	}
}

// The model list is scoped to Gemini: one model, routed back through it.
func TestGeminiModelMenuIsScopedToGemini(t *testing.T) {
	menu := strings.Join(flattenButtons(menus.SubscriptionModelMenu("gemini")), "\n")
	if !strings.Contains(menu, "Gemini 3.8 Flash") {
		t.Errorf("the Gemini model list does not offer its model:\n%s", menu)
	}
	for _, foreign := range []string{"grok", "MiniMax", "claude", "deepseek", "qmodel"} {
		if strings.Contains(menu, foreign) {
			t.Errorf("the Gemini model list contains %q:\n%s", foreign, menu)
		}
	}
	if !strings.Contains(menu, "provider:gemini:model:") {
		t.Errorf("Gemini model buttons do not route back through Gemini:\n%s", menu)
	}
}

// Gemini routes resolve like every other switchable provider.
func TestGeminiCallbackShapes(t *testing.T) {
	for data, kind := range map[string]providerRouteKind{
		"provider:gemini":         providerRouteActivate,
		"provider:gemini:models":  providerRouteModels,
		"provider:gemini:relogin": providerRouteRelogin,
	} {
		provider, got, _ := routeProviderCallback(data)
		if provider != "gemini" || got != kind {
			t.Errorf("%q routed to (%q, %v); the button is dead", data, provider, got)
		}
	}
}

// A login that succeeds but leaves Google asking for account verification is
// two steps, not one: the flow must name the second step, show the link, and
// not announce that Gemini is ready to answer.
func TestGeminiCodeStageCapturesVerificationStep(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureGeminiNotify(t)
	if err := providers.setActive("opencode"); err != nil {
		t.Fatal(err)
	}
	original := geminiExchange
	geminiExchange = func(ctx context.Context, flow *geminiauth.Flow, code string) error { return nil }
	t.Cleanup(func() { geminiExchange = original })
	stubGeminiAccountCheck(t, &llm.CodeAssistValidationError{
		Message:  "Verify your account to continue.",
		URL:      "https://accounts.google.com/signin/continue?sarp=1",
		LinkText: "Verify your account",
	}, nil)

	trackGeminiFlow(5151, geminiFlowOwner{chatID: 5151, activate: true, activeAtStart: "opencode", flow: &geminiauth.Flow{}})
	pendingProvider.Store(5151, providerDraft{ID: "gemini", Stage: "code"})
	if !handleGeminiCodeStage("test-token", 5151, "code-abc") {
		t.Fatal("code stage declined the paste")
	}
	if got := providers.active(); got != "gemini" {
		t.Errorf("active = %q; the verified provider must still become active", got)
	}
	joined := strings.Join(seen.snapshot(), "\n")
	if !strings.Contains(joined, "Step 2 of 2") || !strings.Contains(joined, "accounts.google.com/signin/continue") {
		t.Errorf("the second step was not captured:\n%v", seen.snapshot())
	}
	if strings.Contains(joined, "Gemini provider activated") {
		t.Errorf("the flow claimed Gemini was ready before verification:\n%v", seen.snapshot())
	}
}

// The verification button re-runs the probe. A verified account is reported
// ready; a still-unverified one keeps the link.
func TestGeminiVerifiedCallbackChecksAgain(t *testing.T) {
	seen := captureGeminiNotify(t)
	stubGeminiAccountCheck(t, nil, nil)

	var query callbackQuery
	if err := json.Unmarshal([]byte(`{"id":"1","data":"gemini:verified","message":{"chat":{"id":5151},"message_id":5}}`), &query); err != nil {
		t.Fatalf("callback: %v", err)
	}
	handleCallback("test-token", &query)
	if joined := strings.Join(seen.snapshot(), "\n"); !strings.Contains(joined, "verified and ready") {
		t.Errorf("gemini:verified did not confirm the account; notified:\n%v", seen.snapshot())
	}
}

// Pressing the check before Google clears the validation must not claim
// success; it repeats the link.
func TestGeminiVerifiedCallbackRepeatsPendingLink(t *testing.T) {
	seen := captureGeminiNotify(t)
	stubGeminiAccountCheck(t, &llm.CodeAssistValidationError{
		Message:  "Verify your account to continue.",
		URL:      "https://accounts.google.com/signin/continue?sarp=1",
		LinkText: "Verify your account",
	}, nil)

	var query callbackQuery
	if err := json.Unmarshal([]byte(`{"id":"1","data":"gemini:verified","message":{"chat":{"id":5151},"message_id":5}}`), &query); err != nil {
		t.Fatalf("callback: %v", err)
	}
	handleCallback("test-token", &query)
	joined := strings.Join(seen.snapshot(), "\n")
	if !strings.Contains(joined, "still reports") || !strings.Contains(joined, "accounts.google.com/signin/continue") {
		t.Errorf("a pending account was reported as verified:\n%v", seen.snapshot())
	}
}

func stubGeminiAccountCheck(t *testing.T, validation *llm.CodeAssistValidationError, err error) {
	t.Helper()
	original := geminiAccountCheck
	geminiAccountCheck = func(context.Context) (*llm.CodeAssistValidationError, error) {
		return validation, err
	}
	t.Cleanup(func() { geminiAccountCheck = original })
}
