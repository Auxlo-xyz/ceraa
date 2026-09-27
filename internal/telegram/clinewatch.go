package telegram

import (
	"sync"

	"ceraa/internal/clineauth"
	"ceraa/internal/logger"
	"ceraa/internal/oauthdevice"
)

// The Cline device login has to finish on its own, like Codex and Hermes do.
//
// The user approves the code on accounts.x.ai and then gets on with their task.
// Without a completion callback the credential arrives silently and the provider
// never activates, so the next message runs on whatever was selected before.

// clineFlowOwner records which chat started a login and what it wanted from it.
type clineFlowOwner struct {
	chatID   int64
	activate bool
}

var clineFlows sync.Map

// trackClineFlow records the owner of a flow so its outcome can be reported.
func trackClineFlow(flowID string, chatID int64, activate bool) {
	if flowID == "" {
		return
	}
	clineFlows.Store(flowID, clineFlowOwner{chatID: chatID, activate: activate})
}

func clineFlowOwnerOf(flowID string) (clineFlowOwner, bool) {
	value, ok := clineFlows.Load(flowID)
	if !ok {
		return clineFlowOwner{}, false
	}
	owner, ok := value.(clineFlowOwner)
	return owner, ok
}

func clearClineFlow(flowID string) { clineFlows.Delete(flowID) }

// clineLogin owns the device-login manager and persists the token on success.
var clineLogin = clineauth.NewLogin(nil)

// registerClineCompletion wires the manager's completion callback to the reporting.
func registerClineCompletion(token string) {
	clineLogin.Manager().SetOnComplete(func(snapshot oauthdevice.Snapshot) {
		deliverClineTerminal(token, snapshot)
	})
}

// deliverClineTerminal reports one terminal login state to the chat that owns it.
func deliverClineTerminal(token string, snapshot oauthdevice.Snapshot) {
	owner, ok := clineFlowOwnerOf(snapshot.ID)
	if !ok {
		logger.Info("Cline login " + snapshot.ID + " finished as " + snapshot.Status + " with no owning chat")
		return
	}
	clearClineFlow(snapshot.ID)
	switch snapshot.Status {
	case oauthdevice.StatusSucceeded:
		reportClineSignedIn(token, owner, snapshot.ID)
	case oauthdevice.StatusFailed:
		clineNotify(token, owner.chatID, "❌ Cline sign-in did not complete."+clineFailureDetail(snapshot.Message), clineStatusKeyboard(false))
	case oauthdevice.StatusCanceled:
		// Deliberate; the cancelling button already edited its own message.
	}
}

func clineFailureDetail(message string) string {
	if message == "" {
		return ""
	}
	return "\n\n" + message
}

// reportClineSignedIn confirms the login and switches provider when that is what
// the chat asked for.
func reportClineSignedIn(token string, owner clineFlowOwner, flowID string) {
	if owner.activate {
		activateClineProvider(token, owner.chatID, flowID)
		return
	}
	clineNotify(token, owner.chatID,
		"✅ Cline sign-in completed. The Grok credential is stored and ready.\n\n"+
			"Cline is signed in but not the active provider, because this sign-in did not ask to switch.",
		clineStatusKeyboard(false))
}

// clineNotify posts an Cline outcome to a chat. Indirected so a test can observe that
// the user was actually told, which is the requirement rather than only that some
// internal flag flipped.
var clineNotify = func(token string, chatID int64, text string, kb map[string]any) {
	sendHTML(token, chatID, text, kb)
}

// clineStatusKeyboard is the action row for an Cline status report.
func clineStatusKeyboard(signedIn bool) map[string]any {
	row := func(text, data string) map[string]string {
		return map[string]string{"text": text, "callback_data": data}
	}
	if signedIn {
		return map[string]any{"inline_keyboard": [][]map[string]string{
			{row("✅ Switch to Cline", "provider:cline"), row("🔁 Sign in again", "provider:cline")},
		}}
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{row("🔑 Sign in to Cline", "provider:cline")},
	}}
}
