package telegram

import (
	"sync"

	"ceraa/internal/logger"
	"ceraa/internal/minimaxauth"
	"ceraa/internal/oauthdevice"
)

// The MiniMax device login has to finish on its own, like Codex and Hermes do.
//
// The user approves the code on accounts.x.ai and then gets on with their task.
// Without a completion callback the credential arrives silently and the provider
// never activates, so the next message runs on whatever was selected before.

// minimaxFlowOwner records which chat started a login and what it wanted from it.
type minimaxFlowOwner struct {
	chatID   int64
	activate bool
}

var minimaxFlows sync.Map

// trackMinimaxFlow records the owner of a flow so its outcome can be reported.
func trackMinimaxFlow(flowID string, chatID int64, activate bool) {
	if flowID == "" {
		return
	}
	minimaxFlows.Store(flowID, minimaxFlowOwner{chatID: chatID, activate: activate})
}

func minimaxFlowOwnerOf(flowID string) (minimaxFlowOwner, bool) {
	value, ok := minimaxFlows.Load(flowID)
	if !ok {
		return minimaxFlowOwner{}, false
	}
	owner, ok := value.(minimaxFlowOwner)
	return owner, ok
}

func clearMinimaxFlow(flowID string) { minimaxFlows.Delete(flowID) }

// minimaxLogin owns the device-login manager and persists the token on success.
//
// Global region. A Chinese account needs its own manager, since the two are
// separate account systems and a credential does not work on the other's host.
var minimaxLogin = minimaxauth.NewLogin(minimaxauth.RegionGlobal, nil)

// registerMiniMaxCompletion wires the manager's completion callback to the reporting.
func registerMinimaxCompletion(token string) {
	minimaxLogin.Manager().SetOnComplete(func(snapshot oauthdevice.Snapshot) {
		deliverMinimaxTerminal(token, snapshot)
	})
}

// deliverMinimaxTerminal reports one terminal login state to the chat that owns it.
func deliverMinimaxTerminal(token string, snapshot oauthdevice.Snapshot) {
	owner, ok := minimaxFlowOwnerOf(snapshot.ID)
	if !ok {
		logger.Info("MiniMax login " + snapshot.ID + " finished as " + snapshot.Status + " with no owning chat")
		return
	}
	clearMinimaxFlow(snapshot.ID)
	switch snapshot.Status {
	case oauthdevice.StatusSucceeded:
		reportMinimaxSignedIn(token, owner, snapshot.ID)
	case oauthdevice.StatusFailed:
		minimaxNotify(token, owner.chatID, "❌ MiniMax sign-in did not complete."+minimaxFailureDetail(snapshot.Message), minimaxStatusKeyboard(false))
	case oauthdevice.StatusCanceled:
		// Deliberate; the cancelling button already edited its own message.
	}
}

func minimaxFailureDetail(message string) string {
	if message == "" {
		return ""
	}
	return "\n\n" + message
}

// reportMinimaxSignedIn confirms the login and switches provider when that is what
// the chat asked for.
func reportMinimaxSignedIn(token string, owner minimaxFlowOwner, flowID string) {
	if owner.activate {
		activateMinimaxProvider(token, owner.chatID, flowID)
		return
	}
	minimaxNotify(token, owner.chatID,
		"✅ MiniMax sign-in completed. The Grok credential is stored and ready.\n\n"+
			"MiniMax is signed in but not the active provider, because this sign-in did not ask to switch.",
		minimaxStatusKeyboard(false))
}

// minimaxNotify posts an MiniMax outcome to a chat. Indirected so a test can observe that
// the user was actually told, which is the requirement rather than only that some
// internal flag flipped.
var minimaxNotify = func(token string, chatID int64, text string, kb map[string]any) {
	sendHTML(token, chatID, text, kb)
}

// minimaxStatusKeyboard is the action row for an MiniMax status report.
func minimaxStatusKeyboard(signedIn bool) map[string]any {
	row := func(text, data string) map[string]string {
		return map[string]string{"text": text, "callback_data": data}
	}
	if signedIn {
		return map[string]any{"inline_keyboard": [][]map[string]string{
			{row("✅ Switch to MiniMax", "provider:minimax"), row("🔁 Sign in again", "provider:minimax")},
		}}
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{row("🔑 Sign in to MiniMax", "provider:minimax")},
	}}
}
