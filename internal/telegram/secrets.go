package telegram

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"ceraa/harness/redact"
	"ceraa/internal/logger"
	"ceraa/internal/secret"
	"ceraa/internal/telegram/menus"
)

// armSecretRedaction registers every stored value with the scrubber, so anything
// leaving the process is filtered against what the process holds rather than
// against a list of guesses about what a credential looks like.
//
// Called once at startup, and again whenever a secret is set, so the list is
// never behind the store.
func armSecretRedaction() {
	for _, name := range secret.List() {
		if value, err := secret.Get(name); err == nil {
			redact.RegisterLiteral(value)
		}
	}
}

// pendingSecretTTL bounds how long the next message is treated as a secret.
//
// Without it the pending state is a trap: the prompt says "send the value", the
// user walks away, and their next ordinary request is swallowed and stored as a
// key. The same hazard already exists on the other pending states in
// handlePendingInput, which is why the expiry is enforced centrally here rather
// than left to each caller.
const pendingSecretTTL = 5 * time.Minute

// pendingSecret is a capture in progress for one chat.
type pendingSecret struct {
	Name      string
	ExpiresAt time.Time
}

var (
	pendingSecretMu sync.Mutex
	pendingSecrets  = map[int64]pendingSecret{}
)

// secretFlowLog records which transition a capture took, and never the value.
//
// When a capture silently drops the user's message, the only evidence is which
// state was armed at the time. Logging the name and the state is enough to tell
// an expired prompt from a message that never reached this code at all.
func secretFlowLog(chatID int64, step, detail string) {
	logger.Info(fmt.Sprintf("secret capture chat=%d step=%s %s", chatID, step, detail))
}

// setPendingSecret arms the capture for a chat, replacing any earlier one.
func setPendingSecret(chatID int64, name string) {
	pendingSecretMu.Lock()
	defer pendingSecretMu.Unlock()
	pendingSecrets[chatID] = pendingSecret{Name: name, ExpiresAt: time.Now().Add(pendingSecretTTL)}
	secretFlowLog(chatID, "armed", fmt.Sprintf("name=%q ttl=%s", name, pendingSecretTTL))
}

// takePendingSecret returns an armed capture and clears it, or reports false.
//
// Expired entries are cleared and reported as absent, so an abandoned prompt costs
// the user nothing: their next message is an ordinary message again.
func takePendingSecret(chatID int64) (string, bool) {
	pendingSecretMu.Lock()
	defer pendingSecretMu.Unlock()
	entry, ok := pendingSecrets[chatID]
	if !ok {
		secretFlowLog(chatID, "take", "no capture armed; message is not a secret")
		return "", false
	}
	delete(pendingSecrets, chatID)
	if time.Now().After(entry.ExpiresAt) {
		secretFlowLog(chatID, "take", fmt.Sprintf("name=%q expired; message is not a secret", entry.Name))
		return "", false
	}
	secretFlowLog(chatID, "take", fmt.Sprintf("name=%q consumed", entry.Name))
	return entry.Name, true
}

// secretCaptureArmed reports whether a value is awaited, without consuming it.
//
// A consuming check cannot be used to ask "is this armed?", and the difference is
// not academic: consuming it in a check and then handling the real message finds
// nothing, and the value is silently dropped.
func secretCaptureArmed(chatID int64) bool {
	pendingSecretMu.Lock()
	defer pendingSecretMu.Unlock()
	entry, ok := pendingSecrets[chatID]
	return ok && time.Now().Before(entry.ExpiresAt)
}

// clearPendingSecret disarms a capture without consuming it, for the cancel button.
func clearPendingSecret(chatID int64) {
	pendingSecretMu.Lock()
	defer pendingSecretMu.Unlock()
	delete(pendingSecrets, chatID)
}

// storeSecretValue writes a captured value and reports what is safe to say about
// it: the name and the length, never the value.
//
// The redactor is armed with the value on the way in, so if a later error or log
// line happens to contain it, it is already scrubbed.
func storeSecretValue(name, value string) error {
	value = strings.TrimSpace(value)
	if name == "" {
		return fmt.Errorf("no secret name given")
	}
	if value == "" {
		return fmt.Errorf("the value was empty")
	}
	if err := secret.Set(name, value); err != nil {
		return err
	}
	redact.RegisterLiteral(value)
	armSecretRedaction()
	return nil
}

// secretLength describes a stored value without disclosing it.
func secretLength(value string) string {
	return fmt.Sprintf("%d chars", len([]rune(value)))
}

// sendSecretsMenu shows the stored secrets and the choices for adding one.
func sendSecretsMenu(token string, chatID int64) {
	sendMenuMarkup(token, chatID, "🔑 Secrets — names and lengths only, never values", menus.SecretsMenu(menus.StoredUserSecrets()))
}

// promptForSecretValue arms the capture and tells the user what to send next.
//
// The instruction is explicit about what happens to the message, because "delete
// it afterwards" invites the reasonable fear that it is sitting in a log forever.
// It is deleted from the chat, and it never enters the session or a transcript.
func promptForSecretValue(token string, chatID int64, name string) {
	setPendingSecret(chatID, name)
	sendMenuMarkup(token, chatID, fmt.Sprintf(
		"Send the value for %s as your next message.\n\n"+
			"It is stored encrypted, deleted from this chat, and kept out of the "+
			"session and the model's context. Telegram may still hold a copy on "+
			"its own servers.\n\nValid for %d minutes, or press ✕.",
		name, int(pendingSecretTTL.Minutes())), cancelSecretKeyboard())
}

func cancelSecretKeyboard() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "✕ Cancel", "callback_data": "secret:cancel"}},
	}}
}

// handleCapturedSecret stores the message that carried a value.
//
// The name arrives as an argument rather than being taken from the pending map
// again. Taking it twice is not a harmless redundancy: the first take removes the
// entry, so the second finds nothing, the function returns without storing, and
// the value is silently lost while the caller reports the message as handled. The
// value of this function is the whole capture, so the caller that consumed the
// armed entry owns the name and passes it down.
//
// The order is deliberate: store, then delete. The value is already in Telegram's
// hands the moment it is sent, so deleting first buys a smaller window at the cost
// of the worst outcome — a failed store loses the secret outright, while a failed
// delete leaves a message a person can remove by hand. Loss is not recoverable;
// exposure is.
func handleCapturedSecret(token string, chatID, messageID int64, name, text string) {
	text = strings.TrimSpace(text)
	if strings.EqualFold(text, "/cancel") {
		send(token, chatID, "Cancelled. Nothing stored.")
		return
	}
	if err := storeSecretValue(name, text); err != nil {
		send(token, chatID, "Could not store "+name+": "+err.Error())
		return
	}
	// Best effort, and reported honestly: Telegram only allows this for the last
	// 48 hours and refuses outright in some chats, so a failure is information
	// rather than a failure of the capture.
	deleted := deleteTelegramMessage(token, chatID, int(messageID))
	confirm := fmt.Sprintf("🔑 Stored %s (%s).", name, secretLength(text))
	if deleted != nil {
		confirm += "\n\nI could not delete your message (" + deleted.Error() +
			"). It is still in the chat — please delete it yourself. The value is stored safely either way."
	}
	send(token, chatID, confirm)
	sendMenuMarkup(token, chatID, "Secrets", menus.SecretsMenu(menus.StoredUserSecrets()))
}

// The free-form path asks for the name first and the value second, rather than
// parsing "name value" out of one line. A secret containing a space is ordinary,
// and splitting on the first one would store half of it under a name made of the
// other half — a failure with no error message anywhere.
var (
	pendingNameMu sync.Mutex
	pendingNames  = map[int64]time.Time{}
)

func setPendingSecretName(chatID int64) {
	pendingNameMu.Lock()
	defer pendingNameMu.Unlock()
	pendingNames[chatID] = time.Now().Add(pendingSecretTTL)
	secretFlowLog(chatID, "name-armed", "awaiting a secret name")
}

func clearPendingSecretName(chatID int64) {
	pendingNameMu.Lock()
	defer pendingNameMu.Unlock()
	delete(pendingNames, chatID)
}

// armed reports whether a name is awaited, without consuming it.
func secretNameArmed(chatID int64) bool {
	pendingNameMu.Lock()
	defer pendingNameMu.Unlock()
	expiry, ok := pendingNames[chatID]
	return ok && time.Now().Before(expiry)
}

// handleSecretInput consumes a message that is part of a capture, and reports
// whether it did.
//
// The order of the two states matters. A capture in progress means the user is
// sending a value, so it is checked before the name prompt: otherwise typing a
// name after pressing "Another name" would be stored as the value.
func handleSecretInput(token string, chatID, messageID int64, text string) bool {
	if name, armed := takePendingSecret(chatID); armed {
		handleCapturedSecret(token, chatID, messageID, name, text)
		return true
	}
	if secretNameArmed(chatID) {
		name := strings.TrimSpace(text)
		clearPendingSecretName(chatID)
		if name == "" {
			send(token, chatID, "That was empty, so nothing was stored.")
			return true
		}
		if secret.IsInternal(name) {
			// Refuse rather than shadow. ceraa_ and codex_ names are the harness's
			// own credentials, and letting a user set one would either be ignored or
			// quietly break provider auth.
			send(token, chatID, fmt.Sprintf(
				"%s is a reserved prefix used by Ceraa's own provider credentials. "+
					"Pick another name.", name))
			return true
		}
		promptForSecretValue(token, chatID, name)
		return true
	}
	return false
}
