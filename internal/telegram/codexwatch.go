package telegram

import (
	"encoding/json"
	"sync"

	"ceraa/internal/codexauth"
	"ceraa/internal/logger"
)

// The device login has to finish on its own.
//
// Completion used to be detected only in the codex:check callback, so approving
// the code on the OpenAI page changed nothing until the user came back and
// pressed "Check Login Status". A user who approved and then simply typed their
// task kept the previous provider, with no indication a credential had arrived.
//
// The obvious fix, a poller that watches for a completed flow, cannot work: a
// finished flow is removed from the manager's registry the moment it reaches a
// terminal state, so a poller only ever sees "in progress" or "no such flow". The
// manager therefore calls back, and this file is the callback.

// codexSubscription is the provider id Codex registers under.
const codexSubscription = "codex"

// codexNotify posts a Codex outcome to a chat.
//
// Indirected so a test can observe that the user was actually told, which is the
// requirement, rather than only that some internal flag flipped. A login that
// completes silently is the failure this whole change exists to prevent.
var codexNotify = func(token string, chatID int64, text string, kb map[string]any) {
	sendHTML(token, chatID, text, kb)
}

// codexFlowOwner records which chat started a login and what it wanted from it.
type codexFlowOwner struct {
	chatID int64
	// activate means this chat asked for Codex to become the active provider once
	// the credential lands. A login started only to check status does not set it,
	// and the outcome is then reported without switching provider unasked.
	activate bool
}

// codexFlows maps a flow id to the chat that owns it.
var codexFlows sync.Map

// trackCodexFlow records the owner of a flow so its outcome can be reported.
//
// Replaces the old pendingCodexActivation flag, which was keyed by chat and set
// only by the provider menu. A login started from the typed command was therefore
// invisible to the completion path and could never activate anything.
func trackCodexFlow(flowID string, chatID int64, activate bool) {
	if flowID == "" {
		return
	}
	codexFlows.Store(flowID, codexFlowOwner{chatID: chatID, activate: activate})
	pendingCodexActivation.Store(chatID, activate)
}

// codexFlowOwnerOf reports the owner of a flow.
func codexFlowOwnerOf(flowID string) (codexFlowOwner, bool) {
	raw, ok := codexFlows.Load(flowID)
	if !ok {
		return codexFlowOwner{}, false
	}
	owner, ok := raw.(codexFlowOwner)
	return owner, ok
}

// clearCodexFlow forgets a flow once it has been reported, so a repeated callback
// cannot post the same outcome twice.
func clearCodexFlow(flowID string) {
	if owner, ok := codexFlowOwnerOf(flowID); ok {
		pendingCodexActivation.Delete(owner.chatID)
	}
	codexFlows.Delete(flowID)
}

// codexFlowForChat returns the live flow a chat started, if any.
func codexFlowForChat(chatID int64) *codexauth.Flow {
	var found *codexauth.Flow
	codexFlows.Range(func(key, value any) bool {
		owner, ok := value.(codexFlowOwner)
		if !ok || owner.chatID != chatID {
			return true
		}
		flowID, _ := key.(string)
		flow, err := authMgr.Check(flowID)
		if err != nil {
			return true
		}
		found = flow
		return false
	})
	return found
}

// registerCodexCompletion wires the manager's completion callback to the reporting
// below. Called once at startup.
//
// The flow id is taken from the delivered State rather than looked up, because by
// the time the callback runs the flow is already out of the registry and any
// lookup would fail.
func registerCodexCompletion(token string) {
	authMgr.SetOnComplete(func(state codexauth.State) {
		owner, ok := codexFlowOwnerOf(state.FlowID)
		if !ok {
			// Started by something other than a chat button, or already reported.
			// Nothing to say to anyone in particular.
			logger.Info("Codex login " + state.FlowID + " finished as " + state.Status + " with no owning chat")
			return
		}
		clearCodexFlow(state.FlowID)
		switch state.Status {
		case codexauth.StatusSucceeded:
			reportCodexSignedIn(token, owner)
		case codexauth.StatusFailed:
			reportCodexLoginFailed(token, owner.chatID, state)
		case codexauth.StatusCanceled:
			// Deliberate, and the button that did it already edited its own message.
		}
	})
}

// reportCodexSignedIn confirms a completed login and switches provider when that
// is what the chat asked for.
//
// The user is always told. Switching the model answering underneath a
// conversation with nothing in the transcript to explain it is the surprising
// outcome, so a silent success is not an option even when it would be tidier.
func reportCodexSignedIn(token string, owner codexFlowOwner) {
	if owner.activate {
		// activateCodexProvider sends its own confirmation naming the model, so
		// posting another would be a duplicate.
		activateCodexProvider(token, owner.chatID)
		return
	}
	codexNotify(token, owner.chatID,
		"✅ Codex sign-in completed. Access token valid until "+codexExpiryLabel()+".\n\n"+
			"Codex is signed in but not the active provider, because this sign-in did not ask to switch.",
		codexStatusKeyboard(true, false))
}

// reportCodexLoginFailed explains a failure and leaves a way to retry, rather
// than clearing the buttons and stranding the message.
func reportCodexLoginFailed(token string, chatID int64, state codexauth.State) {
	text := "❌ Codex sign-in did not complete."
	if state.Message != "" {
		text += "\n\n" + state.Message
	}
	if state.Hint != "" {
		text += "\n\n" + state.Hint
	}
	markup, err := codexauth.KeyboardJSONForState(state)
	var kb map[string]any
	if err != nil || json.Unmarshal(markup, &kb) != nil {
		kb = codexStatusKeyboard(codexauth.Authenticated(), false)
	}
	codexNotify(token, chatID, text, kb)
}
