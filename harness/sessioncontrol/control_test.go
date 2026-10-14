package sessioncontrol

import (
	"context"
	"errors"
	"testing"
	"time"

	"ceraa/harness/sessionstore"
)

func TestRetryRefusesUnsafeReplayWithTypedUnsupportedError(t *testing.T) {
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&sessionstore.Session{ID: "s", Attempts: []sessionstore.Attempt{{ID: "a", Kind: "tool", Outcome: sessionstore.OutcomeFailed, ActionRequired: sessionstore.ActionRetry, CreatedAt: time.Now()}}}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Dependencies{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Retry(context.Background(), RetryRequest{SessionID: "s", RequestID: "r", ParentAttempt: "a", Policy: "coordinator-safe"})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Retry error = %v, want ErrUnsupported", err)
	}
	if result.SessionID != "s" || result.ParentAttempt != "a" {
		t.Fatalf("Retry result = %+v, want linked parent projection", result)
	}
	got, err := store.Load("s")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attempts) != 1 {
		t.Fatalf("unsafe retry created attempt: %+v", got.Attempts)
	}
}

func TestRetryValidatesParentRetryabilityBeforeUnsupported(t *testing.T) {
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&sessionstore.Session{ID: "s", Attempts: []sessionstore.Attempt{{ID: "a", Kind: "tool", Outcome: sessionstore.OutcomeSucceeded}}}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Dependencies{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Retry(context.Background(), RetryRequest{SessionID: "s", RequestID: "r", ParentAttempt: "a", Policy: "coordinator-safe"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("non-retryable parent error = %v, want ErrInvalidRequest", err)
	}
}

func TestPauseIsTypedUnsupportedAndNeverCancels(t *testing.T) {
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&sessionstore.Session{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Dependencies{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Pause(context.Background(), PauseRequest{SessionID: "s", RequestID: "p", Reason: "wait"})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Pause error = %v, want ErrUnsupported", err)
	}
	if result.Accepted || result.SessionID != "s" {
		t.Fatalf("Pause result = %+v, want unaccepted typed refusal", result)
	}
}
