package telegram

import (
	"context"
	"sync"

	"ceraa/internal/logger"
	"ceraa/internal/qoderauth"
)

// The Qoder device login finishes on its own: the user approves in the
// browser while the poll runs in the background, and the completion switches
// provider without another tap. Without the report the credential would arrive
// silently and the next message would run on whatever was selected before.

// qoderFlowOwner records which chat started a login and what it wanted.
type qoderFlowOwner struct {
	chatID        int64
	activate      bool
	activeAtStart string
}

var qoderFlows sync.Map

// trackQoderFlow records the owner of a flow so its outcome can be reported.
func trackQoderFlow(flowID string, chatID int64, activate bool, activeAtStart string) {
	if flowID == "" {
		return
	}
	qoderFlows.Store(flowID, qoderFlowOwner{
		chatID: chatID, activate: activate, activeAtStart: activeAtStart,
	})
}

func qoderFlowOwnerOf(flowID string) (qoderFlowOwner, bool) {
	value, ok := qoderFlows.Load(flowID)
	if !ok {
		return qoderFlowOwner{}, false
	}
	owner, ok := value.(qoderFlowOwner)
	return owner, ok
}

func clearQoderFlow(flowID string) { qoderFlows.Delete(flowID) }

// qoderBeginLogin mints a login flow without touching the network: Start only
// builds the approval URL locally. Split out so tests can stub the manager
// without reaching the live service.
var qoderBeginLogin = func() (*qoderauth.Manager, *qoderauth.Flow, error) {
	mgr := qoderauth.NewManager()
	flow, err := mgr.Start()
	if err != nil {
		return nil, nil, err
	}
	return mgr, flow, nil
}

// qoderPollLogin blocks until approval, expiry or failure. Split out so tests
// can settle a flow without polling the live service for five minutes.
var qoderPollLogin = func(mgr *qoderauth.Manager, flow *qoderauth.Flow) error {
	return mgr.Poll(context.Background(), flow)
}

// deliverQoderTerminal reports one terminal login state to the chat that owns
// it. There is no cancel button on the login message: the approval expires in
// five minutes on its own, and an unhandled cancel callback would be a dead
// button, which is worse than none.
func deliverQoderTerminal(token string, flowID string, succeeded bool, message string) {
	owner, ok := qoderFlowOwnerOf(flowID)
	if !ok {
		logger.Info("Qoder login " + flowID + " finished with no owning chat")
		return
	}
	clearQoderFlow(flowID)
	if !succeeded {
		qoderNotify(token, owner.chatID, "❌ Qoder sign-in did not complete."+qoderFailureDetail(message), qoderStatusKeyboard(false))
		return
	}
	if flowActivationStillWanted("Qoder", owner.activate, owner.activeAtStart) {
		activateQoderProvider(token, owner.chatID)
		return
	}
	qoderNotify(token, owner.chatID,
		"✅ Qoder sign-in completed. The credential is stored and ready.\n\n"+
			flowNotActiveReason("Qoder", owner.activate),
		qoderStatusKeyboard(false))
}

func qoderFailureDetail(message string) string {
	if message == "" {
		return ""
	}
	return "\n\n" + message
}

// qoderNotify posts a Qoder outcome to a chat. Indirected so a test can observe
// that the user was actually told, which is the requirement rather than only
// that some internal flag flipped.
var qoderNotify = func(token string, chatID int64, text string, kb map[string]any) {
	sendHTML(token, chatID, text, kb)
}

// qoderStatusKeyboard is the action row for a Qoder status report.
func qoderStatusKeyboard(signedIn bool) map[string]any {
	row := func(text, data string) map[string]string {
		return map[string]string{"text": text, "callback_data": data}
	}
	if signedIn {
		return map[string]any{"inline_keyboard": [][]map[string]string{
			{row("✅ Switch to Qoder", "provider:qoder"), row("🔁 Sign in again", "provider:qoder:relogin")},
		}}
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{row("🔑 Sign in to Qoder", "provider:qoder")},
	}}
}
