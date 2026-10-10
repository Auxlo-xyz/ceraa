package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"ceraa/harness/llm"
)

func captureTraeNotify(t *testing.T) *[]string {
	t.Helper()
	seen := &[]string{}
	var mu sync.Mutex
	original := traeNotify
	traeNotify = func(token string, chatID int64, text string) {
		mu.Lock()
		*seen = append(*seen, text)
		mu.Unlock()
	}
	t.Cleanup(func() { traeNotify = original })
	return seen
}

func stubTraeSavePAT(t *testing.T, err error) {
	t.Helper()
	original := traeSavePAT
	traeSavePAT = func(ctx context.Context, pat string) error { return err }
	t.Cleanup(func() { traeSavePAT = original })
}

// The button existing and being handled are separate facts.
func TestProviderTraeCallbackReachesTheEntryPoint(t *testing.T) {
	called := 0
	original := traeEntryPoint
	traeEntryPoint = func(token string, chatID int64) { called++ }
	t.Cleanup(func() { traeEntryPoint = original })

	var query callbackQuery
	if err := json.Unmarshal([]byte(`{"id":"1","data":"provider:trae","message":{"chat":{"id":5151},"message_id":5}}`), &query); err != nil {
		t.Fatalf("callback: %v", err)
	}
	handleCallback("test-token", &query)

	if called != 1 {
		t.Errorf("provider:trae reached the entry point %d time(s); the button is dead", called)
	}
}

// A pasted PAT that exchanges activates the provider and reports it.
func TestTraePATStageExchangesAndActivates(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureTraeNotify(t)
	stubTraeSavePAT(t, nil)
	pendingProvider.Store(int64(79), providerDraft{ID: llm.TraeSubscription, URL: llm.TraeBaseURL, Stage: "pat"})
	defer pendingProvider.Delete(int64(79))

	if !handlePendingInput("test-token", int64(79), "pasted-pat-value") {
		t.Fatal("the pat stage did not consume the message")
	}
	joined := strings.Join(*seen, "\n")
	if !strings.Contains(joined, "activated") {
		t.Fatalf("activation was not reported: %v", *seen)
	}
	if _, ok := pendingProvider.Load(79); ok {
		t.Error("the draft survived a completed login")
	}
}

// A PAT the platform refuses ends the attempt with the reason, and nothing
// is left behind for the next message to trip over.
func TestTraePATStageFailureExplainsItself(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureTraeNotify(t)
	stubTraeSavePAT(t, errors.New("code 4001: invalid token"))
	pendingProvider.Store(int64(80), providerDraft{ID: llm.TraeSubscription, URL: llm.TraeBaseURL, Stage: "pat"})
	defer pendingProvider.Delete(int64(80))

	if !handlePendingInput("test-token", int64(80), "bogus-pat") {
		t.Fatal("the pat stage did not consume the message")
	}
	joined := strings.Join(*seen, "\n")
	if !strings.Contains(joined, "did not work") {
		t.Fatalf("the failure was not explained: %v", *seen)
	}
	if _, ok := pendingProvider.Load(80); ok {
		t.Error("the draft survived a failed login")
	}
}

// Cancel is a first-class answer, not an invalid PAT.
func TestTraePATStageCancelClearsEverything(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureTraeNotify(t)
	pendingProvider.Store(int64(81), providerDraft{ID: llm.TraeSubscription, URL: llm.TraeBaseURL, Stage: "pat"})
	defer pendingProvider.Delete(int64(81))

	if !handlePendingInput("test-token", int64(81), "cancel") {
		t.Fatal("the pat stage did not consume the message")
	}
	joined := strings.Join(*seen, "\n")
	if !strings.Contains(joined, "cancelled") {
		t.Fatalf("cancel was not confirmed: %v", *seen)
	}
	if _, ok := pendingProvider.Load(81); ok {
		t.Error("the draft survived a cancel")
	}
}
