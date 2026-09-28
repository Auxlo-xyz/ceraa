package telegram

import (
	"path/filepath"
	"testing"

	"ceraa/internal/secret"
)

func TestSecretCaptureFlow(t *testing.T) {
	// A chat id nothing else in the suite uses, so a leaked pending entry from
	// another test cannot make this one pass for the wrong reason.
	const chat = int64(424242)

	// The store is a package-level singleton, so point it at a temp file rather
	// than the operator's real secrets.
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("init store: %v", err)
	}
	const (
		name  = "GitHub_token"
		value = "ghp_secretvalue"
	)
	t.Cleanup(func() {
		clearPendingSecret(chat)
		clearPendingSecretName(chat)
	})
	clearPendingSecret(chat)
	clearPendingSecretName(chat)

	// Step 1: the free-form button arms the name prompt.
	setPendingSecretName(chat)
	if !secretNameArmed(chat) {
		t.Fatal("after setPendingSecretName, secretNameArmed is false")
	}

	// Step 2: the user sends the name.
	if handled := handleSecretInput("tok", chat, 1, "GitHub_token"); !handled {
		t.Fatal("the name message was not handled")
	}
	if secretNameArmed(chat) {
		t.Error("the name prompt is still armed after being consumed")
	}
	// This is the arming the value message depends on.
	if !secretCaptureArmed(chat) {
		t.Fatal("after the name, the value capture is not armed — this is the bug")
	}

	// Step 3: the user sends the value.
	if handled := handleSecretInput("tok", chat, 2, "ghp_secretvalue"); !handled {
		t.Fatal("the value message was not handled")
	}
	if secretCaptureArmed(chat) {
		t.Error("the capture is still armed after being consumed")
	}

	// Step 4: the value is actually in the store.
	//
	// Asserting only the state transitions is what let a real bug through: the
	// capture was consumed, the handler reported the message handled, and nothing
	// was ever stored. A test that checks the arms and stops cannot see that, so
	// this one ends on the value the user would have to retype.
	got, err := secret.Get(name)
	if err != nil {
		t.Fatalf("the value was reported as handled but is not in the store: %v", err)
	}
	if got != value {
		t.Fatalf("stored value = %q, want %q", got, value)
	}
}
