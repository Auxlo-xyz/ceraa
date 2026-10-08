package telegram

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/claudeauth"
	"ceraa/internal/telegram/menus"
)

func captureClaudeNotify(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	claudeNotify = func(token string, chatID int64, text string, kb map[string]any) {
		mu.Lock()
		seen = append(seen, text)
		mu.Unlock()
	}
	t.Cleanup(func() {
		claudeNotify = func(token string, chatID int64, text string, kb map[string]any) {
			sendHTML(token, chatID, text, kb)
		}
	})
	return &seen
}

func stubClaudeExchange(t *testing.T, err error) {
	t.Helper()
	original := claudeExchange
	claudeExchange = func(context.Context, *claudeauth.Flow, string) error { return err }
	t.Cleanup(func() { claudeExchange = original })
}

func stubClaudeStart(t *testing.T, flow *claudeauth.Flow, err error) {
	t.Helper()
	original := claudeStartLoginFlow
	claudeStartLoginFlow = func() (*claudeauth.Flow, error) { return flow, err }
	t.Cleanup(func() { claudeStartLoginFlow = original })
}

func testClaudeFlow() *claudeauth.Flow {
	return &claudeauth.Flow{
		AuthorizeURL: "https://claude.ai/oauth/authorize?test=1",
		Verifier:     "test-verifier",
		State:        "test-state",
	}
}

// The button existing and being handled are separate facts.
func TestProviderClaudeCallbackReachesTheEntryPoint(t *testing.T) {
	var mu sync.Mutex
	called := 0
	original := claudeEntryPoint
	claudeEntryPoint = func(token string, chatID int64) {
		mu.Lock()
		called++
		mu.Unlock()
	}
	t.Cleanup(func() { claudeEntryPoint = original })

	var query callbackQuery
	if err := json.Unmarshal([]byte(`{"id":"1","data":"provider:claude","message":{"chat":{"id":5151},"message_id":5}}`), &query); err != nil {
		t.Fatalf("callback: %v", err)
	}
	handleCallback("test-token", &query)

	mu.Lock()
	defer mu.Unlock()
	if called != 1 {
		t.Errorf("provider:claude reached the entry point %d time(s); the button is dead", called)
	}
}

func TestClaudeModelMenuIsScopedToClaude(t *testing.T) {
	claude := strings.Join(flattenButtons(menus.SubscriptionModelMenu("claude")), "\n")
	if !strings.Contains(claude, "claude-sonnet-4-5") || !strings.Contains(claude, "claude-opus-4-1") {
		t.Errorf("the Claude model list does not offer the account catalog:\n%s", claude)
	}
	for _, foreign := range []string{"MiniMax", "grok", "gpt-6", "deepseek"} {
		if strings.Contains(claude, foreign) {
			t.Errorf("the Claude model list contains %q:\n%s", foreign, claude)
		}
	}
	if !strings.Contains(claude, "provider:claude:model:") {
		t.Errorf("Claude model buttons do not route back through Claude:\n%s", claude)
	}
}

// A signed-in account switches immediately; the browser flow must not start.
func TestClaudeEntryStartsLoginWhileSignedOut(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	started := false
	original := claudeStartLoginFlow
	claudeStartLoginFlow = func() (*claudeauth.Flow, error) {
		started = true
		return testClaudeFlow(), nil
	}
	t.Cleanup(func() { claudeStartLoginFlow = original })

	// SignedIn reads the encrypted store; with an isolated store it is false,
	// so this exercises the login branch. The signed-in branch is covered by
	// activation below.
	claudeEntryPoint("test-token", 77)
	if !started {
		t.Error("the entry point did not start a login while signed out")
	}
	clearClaudeFlow(int64(77))
	pendingProvider.Delete(int64(77))
}

// Starting tells the operator exactly what to do: open, approve, paste.
func TestClaudeStartSendsTheAuthorizeURL(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureClaudeNotify(t)
	stubClaudeStart(t, testClaudeFlow(), nil)

	startClaudeLogin("test-token", 78)
	if len(*seen) != 1 {
		t.Fatalf("got %d messages, want the instructions", len(*seen))
	}
	if !strings.Contains((*seen)[0], "https://claude.ai/oauth/authorize") {
		t.Errorf("the instructions do not contain the authorize URL: %q", (*seen)[0])
	}
	if _, ok := claudeFlowOwnerOf(int64(78)); !ok {
		t.Error("the flow was not tracked for the chat")
	} else {
		clearClaudeFlow(int64(78))
	}
	pendingProvider.Delete(int64(78))
}

// A pasted code that exchanges activates the provider and reports it.
func TestClaudeCodeStageExchangesAndActivates(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureClaudeNotify(t)
	stubClaudeExchange(t, nil)
	trackClaudeFlow(int64(79), claudeFlowOwner{chatID: int64(79), activate: true, activeAtStart: "", flow: testClaudeFlow()})
	defer clearClaudeFlow(int64(79))
	pendingProvider.Store(int64(79), providerDraft{ID: llm.ClaudeSubscription, Stage: "code"})
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
	if _, ok := claudeFlowOwnerOf(int64(79)); ok {
		t.Error("the flow survived a completed login")
	}
}

// A code the platform refuses ends the attempt with the reason, and nothing
// is left behind for the next message to trip over.
func TestClaudeCodeStageFailureExplainsItself(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureClaudeNotify(t)
	stubClaudeExchange(t, &claudeauth.Error{Code: claudeauth.CodeProtocol, Message: "refused"})
	trackClaudeFlow(int64(80), claudeFlowOwner{chatID: int64(80), activate: true, activeAtStart: "", flow: testClaudeFlow()})
	pendingProvider.Store(int64(80), providerDraft{ID: llm.ClaudeSubscription, Stage: "code"})

	if !handlePendingInput("test-token", int64(80), "bad-code") {
		t.Fatal("the code stage did not consume the message")
	}
	if len(*seen) != 1 || !strings.Contains((*seen)[0], "did not complete") {
		t.Fatalf("the failure was not explained: %v", *seen)
	}
	if _, ok := pendingProvider.Load(80); ok {
		t.Error("the draft survived a failed login")
	}
	if _, ok := claudeFlowOwnerOf(int64(80)); ok {
		t.Error("the flow survived a failed login")
	}
}

// Cancel is a first-class answer, not an invalid code.
func TestClaudeCodeStageCancelClearsEverything(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureClaudeNotify(t)
	trackClaudeFlow(int64(81), claudeFlowOwner{chatID: int64(81), activate: true, activeAtStart: "", flow: testClaudeFlow()})
	pendingProvider.Store(int64(81), providerDraft{ID: llm.ClaudeSubscription, Stage: "code"})

	if !handlePendingInput("test-token", int64(81), "cancel") {
		t.Fatal("the code stage did not consume the message")
	}
	if len(*seen) != 1 || !strings.Contains((*seen)[0], "cancelled") {
		t.Fatalf("the cancel was not confirmed: %v", *seen)
	}
	if _, ok := pendingProvider.Load(81); ok {
		t.Error("the draft survived a cancel")
	}
	if _, ok := claudeFlowOwnerOf(int64(81)); ok {
		t.Error("the flow survived a cancel")
	}
}

// A draft with no flow is a leftover, not a login. It must say so rather
// than exchange against nothing.
func TestClaudeCodeStageWithoutFlowAsksForAStart(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureClaudeNotify(t)
	pendingProvider.Store(int64(82), providerDraft{ID: llm.ClaudeSubscription, Stage: "code"})
	defer pendingProvider.Delete(int64(82))

	if !handlePendingInput("test-token", int64(82), "orphaned-code") {
		t.Fatal("the code stage did not consume the message")
	}
	if len(*seen) != 1 || !strings.Contains((*seen)[0], "No Claude login") {
		t.Fatalf("an orphaned code did not ask for a fresh start: %v", *seen)
	}
}

// The model follows the provider from one source of truth: the current model
// survives a switch only when the new provider serves it, otherwise the
// provider default. A pinned model from the previous provider is how a switch
// crashes the first run.
func TestClaudeActivationInheritsAServedModel(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	prevModel, hadModel := os.LookupEnv("CERAA_MODEL")
	defer func() {
		if hadModel {
			os.Setenv("CERAA_MODEL", prevModel)
		} else {
			os.Unsetenv("CERAA_MODEL")
		}
	}()
	if err := providers.setModel("claude-sonnet-4-5"); err != nil {
		t.Fatal(err)
	}
	if got := claudeModelForAccount(); got != "claude-sonnet-4-5" {
		t.Fatalf("claudeModelForAccount = %q, want the served current model", got)
	}
	if err := providers.setModel("deepseek-flash"); err != nil {
		t.Fatal(err)
	}
	if got := claudeModelForAccount(); got != llm.DefaultClaudeModel {
		t.Fatalf("claudeModelForAccount = %q, want the default for a foreign model", got)
	}
}
