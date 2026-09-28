package telegram

import "testing"

func TestSecretCaptureFlow(t *testing.T) {
	// A chat id nothing else in the suite uses, so a leaked pending entry from
	// another test cannot make this one pass for the wrong reason.
	const chat = int64(424242)
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
}
