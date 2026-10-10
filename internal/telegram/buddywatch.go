package telegram

import (
	"context"
	"sync"

	"ceraa/internal/buddyauth"
	"ceraa/internal/logger"
)

// CodeBuddy and WorkBuddy logins finish on their own: the user approves in
// the browser while the poll runs in the background, and the completion
// switches provider without another tap. One watch serves both products; the
// owner records which product started the flow because the two are separate
// account systems and activating the wrong one spends a good credential on the
// wrong host.

// buddyFlowOwner records which chat started a login and what it wanted.
type buddyFlowOwner struct {
	chatID        int64
	activate      bool
	activeAtStart string
	productID     string
}

var buddyFlows sync.Map

// trackBuddyFlow records the owner of a flow so its outcome can be reported.
func trackBuddyFlow(flowID string, chatID int64, activate bool, activeAtStart, productID string) {
	if flowID == "" {
		return
	}
	buddyFlows.Store(flowID, buddyFlowOwner{
		chatID: chatID, activate: activate, activeAtStart: activeAtStart, productID: productID,
	})
}

func buddyFlowOwnerOf(flowID string) (buddyFlowOwner, bool) {
	value, ok := buddyFlows.Load(flowID)
	if !ok {
		return buddyFlowOwner{}, false
	}
	owner, ok := value.(buddyFlowOwner)
	return owner, ok
}

func clearBuddyFlow(flowID string) { buddyFlows.Delete(flowID) }

// buddyBeginLogin mints a login flow without polling it. Start reaches the
// network once for the login state; the wait happens in buddyPollLogin.
// Split out so tests can stub the manager without reaching the live service.
var buddyBeginLogin = func(mgr *buddyauth.Manager) (*buddyauth.Flow, error) {
	return mgr.Start(context.Background())
}

// buddyPollLogin blocks until approval, expiry or failure. Split out so tests
// can settle a flow without polling the live service for five minutes.
var buddyPollLogin = func(mgr *buddyauth.Manager, flow *buddyauth.Flow) error {
	return mgr.Poll(context.Background(), flow)
}

func buddyProductFor(id string) buddyauth.Product {
	if id == "workbuddy" {
		return buddyauth.WorkBuddyProduct()
	}
	return buddyauth.BuddyProduct()
}

// deliverBuddyTerminal reports one terminal login state to the chat that owns
// it. There is no cancel button on the login message: the approval expires in
// five minutes on its own, and an unhandled cancel callback would be a dead
// button, which is worse than none.
func deliverBuddyTerminal(token string, flowID string, succeeded bool, message string) {
	owner, ok := buddyFlowOwnerOf(flowID)
	if !ok {
		logger.Info("Buddy login " + flowID + " finished with no owning chat")
		return
	}
	clearBuddyFlow(flowID)
	product := buddyProductFor(owner.productID)
	name := product.ID
	if !succeeded {
		buddyNotify(token, owner.chatID, "❌ "+name+" sign-in did not complete."+buddyFailureDetail(message), buddyStatusKeyboard(product, false))
		return
	}
	if flowActivationStillWanted(name, owner.activate, owner.activeAtStart) {
		activateBuddyProvider(token, owner.chatID, product)
		return
	}
	buddyNotify(token, owner.chatID,
		"✅ "+name+" sign-in completed. The credential is stored and ready.\n\n"+
			flowNotActiveReason(name, owner.activate),
		buddyStatusKeyboard(product, false))
}

func buddyFailureDetail(message string) string {
	if message == "" {
		return ""
	}
	return "\n\n" + message
}

// buddyNotify posts a buddy outcome to a chat. Indirected so a test can
// observe that the user was actually told, which is the requirement rather
// than only that some internal flag flipped.
var buddyNotify = func(token string, chatID int64, text string, kb map[string]any) {
	sendHTML(token, chatID, text, kb)
}

// buddyStatusKeyboard is the action row for a buddy status report.
func buddyStatusKeyboard(product buddyauth.Product, signedIn bool) map[string]any {
	row := func(text, data string) map[string]string {
		return map[string]string{"text": text, "callback_data": data}
	}
	entry := "provider:" + product.ID
	if signedIn {
		return map[string]any{"inline_keyboard": [][]map[string]string{
			{row("✅ Switch to "+product.ID, entry), row("🔁 Sign in again", entry+":relogin")},
		}}
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{row("🔑 Sign in to "+product.ID, entry)},
	}}
}
