package telegram

import (
	"sync"

	"ceraa/internal/logger"
	"ceraa/internal/oauthdevice"
	"ceraa/internal/xaiauth"
)

// The xAI device login has to finish on its own, like Codex and Hermes do.
//
// The user approves the code on accounts.x.ai and then gets on with their task.
// Without a completion callback the credential arrives silently and the provider
// never activates, so the next message runs on whatever was selected before.

// xaiFlowOwner records which chat started a login and what it wanted from it.
type xaiFlowOwner struct {
	chatID   int64
	activate bool
	// activeAtStart is the provider that was selected when this sign-in began, so
	// the completion can tell whether the switch it was asked to make is still
	// wanted. Read by the caller before the flow starts; see the MiniMax watcher.
	activeAtStart string
}

var xaiFlows sync.Map

// trackXaiFlow records the owner of a flow so its outcome can be reported.
//
// Nothing is read here: the owner is stored in one map write, because the login
// may already have finished and be waiting for an owner to talk to.
func trackXaiFlow(flowID string, chatID int64, activate bool, activeAtStart string) {
	if flowID == "" {
		return
	}
	xaiFlows.Store(flowID, xaiFlowOwner{
		chatID: chatID, activate: activate, activeAtStart: activeAtStart,
	})
}

func xaiFlowOwnerOf(flowID string) (xaiFlowOwner, bool) {
	value, ok := xaiFlows.Load(flowID)
	if !ok {
		return xaiFlowOwner{}, false
	}
	owner, ok := value.(xaiFlowOwner)
	return owner, ok
}

func clearXaiFlow(flowID string) { xaiFlows.Delete(flowID) }

// xaiLogin owns the device-login manager and persists the token on success.
var xaiLogin = xaiauth.NewLogin(nil)

// registerXaiCompletion wires the manager's completion callback to the reporting.
func registerXaiCompletion(token string) {
	xaiLogin.Manager().SetOnComplete(func(snapshot oauthdevice.Snapshot) {
		deliverXaiTerminal(token, snapshot)
	})
}

// deliverXaiTerminal reports one terminal login state to the chat that owns it.
func deliverXaiTerminal(token string, snapshot oauthdevice.Snapshot) {
	owner, ok := xaiFlowOwnerOf(snapshot.ID)
	if !ok {
		logger.Info("xAI login " + snapshot.ID + " finished as " + snapshot.Status + " with no owning chat")
		return
	}
	clearXaiFlow(snapshot.ID)
	switch snapshot.Status {
	case oauthdevice.StatusSucceeded:
		reportXaiSignedIn(token, owner, snapshot.ID)
	case oauthdevice.StatusFailed:
		xaiNotify(token, owner.chatID, "❌ xAI sign-in did not complete."+xaiFailureDetail(snapshot.Message), xaiStatusKeyboard(false))
	case oauthdevice.StatusCanceled:
		// Deliberate; the cancelling button already edited its own message.
	}
}

func xaiFailureDetail(message string) string {
	if message == "" {
		return ""
	}
	return "\n\n" + message
}

// reportXaiSignedIn confirms the login and switches provider when that is what
// the chat asked for, and when the choice has not since been made afresh.
func reportXaiSignedIn(token string, owner xaiFlowOwner, flowID string) {
	if flowActivationStillWanted("xAI", owner.activate, owner.activeAtStart) {
		activateXaiProvider(token, owner.chatID, flowID)
		return
	}
	xaiNotify(token, owner.chatID,
		"✅ xAI sign-in completed. The credential is stored and ready.\n\n"+
			flowNotActiveReason("xAI", owner.activate),
		xaiStatusKeyboard(false))
}

// xaiNotify posts an xAI outcome to a chat. Indirected so a test can observe that
// the user was actually told, which is the requirement rather than only that some
// internal flag flipped.
var xaiNotify = func(token string, chatID int64, text string, kb map[string]any) {
	sendHTML(token, chatID, text, kb)
}

// xaiStatusKeyboard is the action row for an xAI status report.
func xaiStatusKeyboard(signedIn bool) map[string]any {
	row := func(text, data string) map[string]string {
		return map[string]string{"text": text, "callback_data": data}
	}
	if signedIn {
		return map[string]any{"inline_keyboard": [][]map[string]string{
			{row("✅ Switch to xAI", "provider:xai"), row("🔁 Sign in again", "provider:xai")},
		}}
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{row("🔑 Sign in to xAI", "provider:xai")},
	}}
}
