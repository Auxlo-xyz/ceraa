package sessionstore

import (
	"errors"
	"testing"
)

func TestExternalActionPersistsReplaysAndConflicts(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, replay, err := store.BeginExternalAction("s", ExternalAction{ID: "tg:chat:1:send:7", Kind: "telegram.sendRichMessage", Fingerprint: "sha256:payload"})
	if err != nil || replay || first.Status != ExternalActionPending {
		t.Fatalf("begin = %+v, replay=%v, err=%v", first, replay, err)
	}
	if err := store.CompleteExternalAction("s", first.ID, first.Kind, first.Fingerprint, "message:42", map[string]any{"message_id": 42}); err != nil {
		t.Fatal(err)
	}
	got, replay, err := store.BeginExternalAction("s", ExternalAction{ID: first.ID, Kind: first.Kind, Fingerprint: first.Fingerprint})
	if err != nil || !replay || got.Status != ExternalActionCompleted || got.ExternalID != "message:42" {
		t.Fatalf("replay = %+v, replay=%v, err=%v", got, replay, err)
	}
	_, _, err = store.BeginExternalAction("s", ExternalAction{ID: first.ID, Kind: first.Kind, Fingerprint: "sha256:other"})
	if !errors.Is(err, ErrExternalActionConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	loaded, err := store.Load("s")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.ExternalAction(first.ID); !ok {
		t.Fatal("receipt did not survive reload")
	}
}

func TestExternalActionCompletionIsIdempotentAndConflicting(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	action := ExternalAction{ID: "a", Kind: "telegram.sendMessage", Fingerprint: "f"}
	if _, _, err := store.BeginExternalAction("s", action); err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"message_id": float64(3)}
	if err := store.CompleteExternalAction("s", "a", "telegram.sendMessage", "f", "3", result); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteExternalAction("s", "a", "telegram.sendMessage", "f", "3", result); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteExternalAction("s", "a", "telegram.sendMessage", "f", "4", result); !errors.Is(err, ErrExternalActionConflict) {
		t.Fatalf("expected completion conflict, got %v", err)
	}
}
