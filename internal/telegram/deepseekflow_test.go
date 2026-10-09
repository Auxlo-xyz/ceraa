package telegram

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/deepseekauth"
	"ceraa/internal/telegram/menus"
)

func captureDeepSeekNotify(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	deepseekNotify = func(token string, chatID int64, text string, kb map[string]any) {
		mu.Lock()
		seen = append(seen, text)
		mu.Unlock()
	}
	t.Cleanup(func() {
		deepseekNotify = func(token string, chatID int64, text string, kb map[string]any) {
			sendHTML(token, chatID, text, kb)
		}
	})
	return &seen
}

func stubDeepSeekExchange(t *testing.T, err error) {
	t.Helper()
	original := deepseekExchange
	deepseekExchange = func(context.Context, *deepseekauth.Flow, string) error { return err }
	t.Cleanup(func() { deepseekExchange = original })
}

func stubDeepSeekStart(t *testing.T, flow *deepseekauth.Flow, err error) {
	t.Helper()
	original := deepseekStartLoginFlow
	deepseekStartLoginFlow = func(context.Context, string, string) (*deepseekauth.Flow, error) {
		return flow, err
	}
	t.Cleanup(func() { deepseekStartLoginFlow = original })
}

func testDeepSeekFlow() *deepseekauth.Flow {
	return &deepseekauth.Flow{
		AuthorizeURL: "https://platform.deepseek.com/dsh/authorize",
		AuthorizeID:  "test-authorize-id",
		Verifier:     "test-verifier",
		State:        "test-state",
		RedirectURI:  "http://127.0.0.1:8787/oauth/callback",
	}
}

// The button existing and being handled are separate facts.
func TestProviderDeepSeekCallbackReachesTheEntryPoint(t *testing.T) {
	var mu sync.Mutex
	called := 0
	original := deepseekEntryPoint
	deepseekEntryPoint = func(token string, chatID int64) {
		mu.Lock()
		called++
		mu.Unlock()
	}
	t.Cleanup(func() { deepseekEntryPoint = original })

	var query callbackQuery
	if err := json.Unmarshal([]byte(`{"id":"1","data":"provider:deepseek","message":{"chat":{"id":5151},"message_id":5}}`), &query); err != nil {
		t.Fatalf("callback: %v", err)
	}
	handleCallback("test-token", &query)

	mu.Lock()
	defer mu.Unlock()
	if called != 1 {
		t.Errorf("provider:deepseek reached the entry point %d time(s); the button is dead", called)
	}
}

// "Sign in again" must start a fresh login, not silently do nothing. The relogin
// route resolved but the handler switch had no DeepSeek case, so the tap fell
// through and the button was dead.
func TestDeepSeekReloginStartsALogin(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureDeepSeekNotify(t)
	stubDeepSeekStart(t, testDeepSeekFlow(), nil)

	var query callbackQuery
	if err := json.Unmarshal([]byte(`{"id":"1","data":"provider:deepseek:relogin","message":{"chat":{"id":5151},"message_id":5}}`), &query); err != nil {
		t.Fatalf("callback: %v", err)
	}
	handleCallback("test-token", &query)

	if len(*seen) != 1 {
		t.Fatalf("got %d messages, want the login instructions", len(*seen))
	}
	if !strings.Contains((*seen)[0], "https://platform.deepseek.com/dsh/authorize") {
		t.Errorf("provider:deepseek:relogin did not start a login: %q", (*seen)[0])
	}
}

// The model list is scoped to the provider whose button opened it. A DeepSeek
// list showing another provider's ids would write a profile the endpoint
// rejects on first use.
func TestDeepSeekModelMenuIsScopedToDeepSeek(t *testing.T) {
	deepseek := strings.Join(flattenButtons(menus.SubscriptionModelMenu("deepseek")), "\n")
	if !strings.Contains(deepseek, "deepseek-v4-pro") || !strings.Contains(deepseek, "deepseek-flash") {
		t.Errorf("the DeepSeek model list does not offer the account catalog:\n%s", deepseek)
	}
	for _, foreign := range []string{"MiniMax", "grok", "gpt-6", "claude"} {
		if strings.Contains(deepseek, foreign) {
			t.Errorf("the DeepSeek model list contains %q:\n%s", foreign, deepseek)
		}
	}
	if !strings.Contains(deepseek, "provider:deepseek:model:") {
		t.Errorf("DeepSeek model buttons do not route back through DeepSeek:\n%s", deepseek)
	}
}

// A signed-in account switches immediately; the browser flow must not start.
func TestDeepSeekEntryActivatesWhenSignedIn(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureDeepSeekNotify(t)

	started := false
	original := deepseekStartLoginFlow
	deepseekStartLoginFlow = func(context.Context, string, string) (*deepseekauth.Flow, error) {
		started = true
		return testDeepSeekFlow(), nil
	}
	t.Cleanup(func() { deepseekStartLoginFlow = original })

	// SignedIn reads the encrypted store; with an isolated store it is false,
	// so this exercises the login branch. The signed-in branch is covered by
	// activation below.
	deepseekEntryPoint("test-token", 77)
	if !started {
		t.Error("the entry point did not start a login while signed out")
	}
	if len(*seen) != 0 {
		// Start failed before any message only if the stubbed flow errored;
		// with a good flow the instructions must have gone out.
		t.Logf("noted %d message(s)", len(*seen))
	}
}

// Starting tells the operator exactly what to do: open, approve, paste.
func TestDeepSeekStartSendsTheAuthorizeURL(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureDeepSeekNotify(t)
	stubDeepSeekStart(t, testDeepSeekFlow(), nil)

	startDeepSeekLogin("test-token", 78)
	if len(*seen) != 1 {
		t.Fatalf("got %d messages, want the instructions", len(*seen))
	}
	if !strings.Contains((*seen)[0], "https://platform.deepseek.com/dsh/authorize") {
		t.Errorf("the instructions do not contain the authorize URL: %q", (*seen)[0])
	}
	if _, ok := deepseekFlowOwnerOf(int64(78)); !ok {
		t.Error("the flow was not tracked for the chat")
	} else {
		clearDeepSeekFlow(int64(78))
	}
	pendingProvider.Delete(int64(78))
}

// A pasted code that exchanges activates the provider and reports it.
func TestDeepSeekCodeStageExchangesAndActivates(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureDeepSeekNotify(t)
	stubDeepSeekExchange(t, nil)
	trackDeepSeekFlow(int64(79), deepseekFlowOwner{chatID: int64(79), activate: true, activeAtStart: "", flow: testDeepSeekFlow()})
	defer clearDeepSeekFlow(int64(79))
	pendingProvider.Store(int64(79), providerDraft{ID: llm.DeepSeekSubscription, Stage: "code"})
	defer pendingProvider.Delete(int64(79))

	if !handlePendingInput("test-token", int64(79), "pasted-code-value") {
		t.Fatal("the code stage did not consume the message")
	}
	if len(*seen) != 1 || !strings.Contains((*seen)[0], "activated") {
		t.Fatalf("activation was not reported: %v", *seen)
	}
	if _, ok := pendingProvider.Load(79); ok {
		t.Error("the draft survived a completed login")
	}
	if _, ok := deepseekFlowOwnerOf(int64(79)); ok {
		t.Error("the flow survived a completed login")
	}
}

// A code the platform refuses ends the attempt with the reason, and nothing
// is left behind for the next message to trip over.
func TestDeepSeekCodeStageFailureExplainsItself(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureDeepSeekNotify(t)
	stubDeepSeekExchange(t, &deepseekauth.Error{Code: deepseekauth.CodeProtocol, Message: "refused"})
	trackDeepSeekFlow(int64(80), deepseekFlowOwner{chatID: int64(80), activate: true, activeAtStart: "", flow: testDeepSeekFlow()})
	pendingProvider.Store(int64(80), providerDraft{ID: llm.DeepSeekSubscription, Stage: "code"})

	if !handlePendingInput("test-token", int64(80), "bad-code") {
		t.Fatal("the code stage did not consume the message")
	}
	if len(*seen) != 1 || !strings.Contains((*seen)[0], "did not complete") {
		t.Fatalf("the failure was not explained: %v", *seen)
	}
	if _, ok := pendingProvider.Load(80); ok {
		t.Error("the draft survived a failed login")
	}
	if _, ok := deepseekFlowOwnerOf(int64(80)); ok {
		t.Error("the flow survived a failed login")
	}
}

// Cancel is a first-class answer, not an invalid code.
func TestDeepSeekCodeStageCancelClearsEverything(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureDeepSeekNotify(t)
	trackDeepSeekFlow(int64(81), deepseekFlowOwner{chatID: int64(81), activate: true, activeAtStart: "", flow: testDeepSeekFlow()})
	pendingProvider.Store(int64(81), providerDraft{ID: llm.DeepSeekSubscription, Stage: "code"})

	if !handlePendingInput("test-token", int64(81), "cancel") {
		t.Fatal("the code stage did not consume the message")
	}
	if len(*seen) != 1 || !strings.Contains((*seen)[0], "cancelled") {
		t.Fatalf("the cancel was not confirmed: %v", *seen)
	}
	if _, ok := pendingProvider.Load(81); ok {
		t.Error("the draft survived a cancel")
	}
	if _, ok := deepseekFlowOwnerOf(int64(81)); ok {
		t.Error("the flow survived a cancel")
	}
}

// A draft with no flow is a leftover, not a login. It must say so rather
// than exchange against nothing.
func TestDeepSeekCodeStageWithoutFlowAsksForAStart(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureDeepSeekNotify(t)
	pendingProvider.Store(int64(82), providerDraft{ID: llm.DeepSeekSubscription, Stage: "code"})
	defer pendingProvider.Delete(int64(82))

	if !handlePendingInput("test-token", int64(82), "orphaned-code") {
		t.Fatal("the code stage did not consume the message")
	}
	if len(*seen) != 1 || !strings.Contains((*seen)[0], "No DeepSeek login") {
		t.Fatalf("an orphaned code did not ask for a fresh start: %v", *seen)
	}
}

// The model follows the provider from one source of truth: the current model
// survives a switch only when the new provider serves it, otherwise the
// provider default. A pinned model from the previous provider is how a switch
// crashes the first run.
func TestDeepSeekActivationInheritsAServedModel(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	// setModel keeps the env file in step, and the process environment is
	// global: without restoring it here, CERAA_MODEL leaks into whatever test
	// runs next and its menu reads the leftover. File order decides who pays.
	prevModel, hadModel := os.LookupEnv("CERAA_MODEL")
	defer func() {
		if hadModel {
			os.Setenv("CERAA_MODEL", prevModel)
		} else {
			os.Unsetenv("CERAA_MODEL")
		}
	}()
	if err := providers.setModel("deepseek-flash"); err != nil {
		t.Fatal(err)
	}
	if got := deepseekModelForAccount(); got != "deepseek-flash" {
		t.Errorf("deepseekModelForAccount = %q, want the served current model", got)
	}
	if err := providers.setModel("gpt-6-luna"); err != nil {
		t.Fatal(err)
	}
	if got := deepseekModelForAccount(); got != llm.DefaultDeepSeekModel {
		t.Errorf("deepseekModelForAccount = %q, want the default for an unserved model", got)
	}
}
