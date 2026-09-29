package telegram

import (
	"sync"

	"ceraa/internal/hermesauth"
	"ceraa/internal/logger"
)

// The Hermes device login has to finish on its own, the same way Codex does.
//
// The user approves the code on the Nous portal page and then gets on with their
// task. If completion is only noticed by a "Check Login Status" tap, the
// credential arrives with nothing to show for it and the provider never becomes
// active, so the next task silently runs on whatever was selected before. That is
// the failure this whole file exists to prevent.

// hermesProviderID is the provider id the Hermes profile registers under.
const hermesProviderID = "hermes"

// hermesNotify posts a Hermes outcome to a chat.
//
// Indirected so a test can observe that the user was actually told, which is the
// requirement, rather than only that some internal flag flipped. A login that
// completes silently is the failure this whole file exists to prevent.
var hermesNotify = func(token string, chatID int64, text string, kb map[string]any) {
	sendHTML(token, chatID, text, kb)
}

// hermesFlowOwner records which chat started a login and what it wanted from it.
type hermesFlowOwner struct {
	chatID int64
	// activate means this chat asked for Hermes to become the active provider once
	// the credential lands. A login started only to inspect status does not set
	// it, and the outcome is then reported without switching provider unasked.
	activate bool
	// activeAtStart is the provider that was selected when this sign-in began, so
	// the completion can tell whether the switch it was asked to make is still
	// wanted. Read by the caller before the flow starts; see the MiniMax watcher.
	activeAtStart string
}

// hermesFlows maps a flow id to the chat that owns it.
var hermesFlows sync.Map

// trackHermesFlow records the owner of a flow so its outcome can be reported.
//
// Without this the callback cannot tell which chat to speak to. A login started
// from the provider menu or from /hermes-login is otherwise invisible to the
// completion path, which is why Hermes had no way to activate on its own.
//
// Nothing is read here: the owner is stored in one map write, because the login
// may already have finished and be waiting for an owner to talk to.
func trackHermesFlow(flowID string, chatID int64, activate bool, activeAtStart string) {
	if flowID == "" {
		return
	}
	hermesFlows.Store(flowID, hermesFlowOwner{
		chatID: chatID, activate: activate, activeAtStart: activeAtStart,
	})
}

// hermesFlowOwnerOf reports the owner of a flow.
func hermesFlowOwnerOf(flowID string) (hermesFlowOwner, bool) {
	value, ok := hermesFlows.Load(flowID)
	if !ok {
		return hermesFlowOwner{}, false
	}
	owner, ok := value.(hermesFlowOwner)
	return owner, ok
}

// clearHermesFlow forgets a flow once its outcome has been reported, so a second
// delivery does not post a duplicate message.
func clearHermesFlow(flowID string) {
	hermesFlows.Delete(flowID)
}

// registerHermesCompletion wires the manager's completion callback to the
// reporting below. Called once at startup.
func registerHermesCompletion(token string) {
	hermesAuthMgr.SetOnComplete(func(state hermesauth.TerminalState) {
		deliverHermesTerminal(token, state)
	})
}

// deliverHermesTerminal reports one terminal login state to the chat that owns it.
//
// Split out of the callback because the callback itself cannot be reached from a
// test: registering it hands a closure to the manager, and there is no way to
// make the manager deliver a state on demand. Testing through the reporting
// functions instead meant the "no owning chat" branch, which is the branch that
// decides whether to speak at all, was never executed by anything.
func deliverHermesTerminal(token string, state hermesauth.TerminalState) {
	owner, ok := hermesFlowOwnerOf(state.FlowID)
	if !ok {
		// Started by something other than a chat button, or already reported.
		// Nothing to say to anyone in particular.
		logger.Info("Hermes login " + state.FlowID + " finished as " + state.Status + " with no owning chat")
		return
	}
	clearHermesFlow(state.FlowID)
	switch state.Status {
	case hermesauth.StatusSucceeded:
		reportHermesSignedIn(token, owner, state.FlowID)
	case hermesauth.StatusFailed:
		reportHermesLoginFailed(token, owner.chatID, state)
	case hermesauth.StatusCanceled:
		// Deliberate, and the button that cancelled already edited its own message.
	}
}

// reportHermesSignedIn confirms a completed login and switches provider when that
// is what the chat asked for.
//
// The user is always told. Swapping the model answering underneath a
// conversation with nothing in the transcript to explain it is the surprising
// outcome, so a silent success is not an option even when it would be tidier.
func reportHermesSignedIn(token string, owner hermesFlowOwner, flowID string) {
	if flowActivationStillWanted("Hermes", owner.activate, owner.activeAtStart) {
		// activateHermesProvider sends its own confirmation naming the model, so
		// posting another would be a duplicate.
		activateHermesProvider(token, owner.chatID, flowID)
		return
	}
	hermesNotify(token, owner.chatID,
		"✅ Hermes sign-in completed. The Nous Portal credential is stored and ready.\n\n"+
			flowNotActiveReason("Hermes", owner.activate),
		hermesStatusKeyboard(false))
}

// reportHermesLoginFailed explains a failure and leaves a way to retry, rather
// than clearing the buttons and stranding the message.
func reportHermesLoginFailed(token string, chatID int64, state hermesauth.TerminalState) {
	text := "❌ Hermes sign-in did not complete."
	if state.Message != "" {
		text += "\n\n" + state.Message
	}
	hermesNotify(token, chatID, text, hermesStatusKeyboard(false))
}

// hermesStatusKeyboard is the action row for a Hermes status report.
//
// The status replies used to instruct the user to type /hermes-login. That is a
// dead end for anyone driving the bot from its menus, which is the only surface
// available on a phone, so every state carries a button for what the user wants.
//
// "Switch to Hermes" and "Sign in again" were both provider:hermes, so both took
// the activate branch and a signed-in user asking to re-authenticate was switched
// to the provider they were already on, with no login and no message saying so.
// The router has had a distinct :relogin case since it was extracted, and
// subscriptionMenu has always emitted it; only these four status keyboards were
// left pointing at the shared callback.
func hermesStatusKeyboard(signedIn bool) map[string]any {
	row := func(text, data string) map[string]string {
		return map[string]string{"text": text, "callback_data": data}
	}
	if signedIn {
		return map[string]any{"inline_keyboard": [][]map[string]string{
			{row("✅ Switch to Hermes", "provider:hermes"), row("🔁 Sign in again", "provider:hermes:relogin")},
		}}
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{row("🔑 Sign in to Nous Portal", "provider:hermes")},
	}}
}
