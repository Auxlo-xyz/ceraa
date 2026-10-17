// Tests for LID-to-phone rewriting on inbound direct messages.
//
// The allow-list holds phone numbers, but WhatsApp addresses most DMs by
// LID now. The digits of a LID and of a number are different namespaces, so
// without the rewrite a listed owner is refused in silence.
package whatsapp

import (
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func lidJID() types.JID {
	jid := types.NewJID("193471331729523", types.HiddenUserServer)
	jid.Device = 8
	return jid
}

func phoneJID() types.JID {
	jid := types.NewJID("2347034215810", types.DefaultUserServer)
	jid.Device = 8
	return jid
}

func TestALIDAddressedMessageTakesThePhoneForm(t *testing.T) {
	got := phoneAddressed(lidJID().String(), phoneJID())
	if got != "2347034215810@s.whatsapp.net" {
		t.Fatalf("LID chat must take the phone form, got %q", got)
	}
	listed := Config{AllowJIDs: []string{"2347034215810"}}
	if !listed.Allowed(got, "2349073100938@s.whatsapp.net") {
		t.Fatalf("the phone form must match the number on the allow-list")
	}
}

// The control: the refusal the owner actually hit. If this ever passes the
// rewrite has stopped mattering, and the fix needs re-reading rather than
// keeping.
func TestALIDStillFailsAgainstAPhoneAllowList(t *testing.T) {
	listed := Config{AllowJIDs: []string{"2347034215810"}}
	if listed.Allowed(lidJID().String(), "2349073100938@s.whatsapp.net") {
		t.Fatalf("a raw LID must not match a phone-number allow entry")
	}
}

func TestAnUnresolvedLIDStaysClosed(t *testing.T) {
	if got := phoneAddressed(lidJID().String(), types.JID{}); got != lidJID().String() {
		t.Fatalf("an absent alternate must keep the LID, got %q", got)
	}
	if got := phoneAddressed(lidJID().String(), types.NewJID("83314262155432", types.HiddenUserServer)); got != lidJID().String() {
		t.Fatalf("a LID alternate is not a phone form, got %q", got)
	}
}

func TestAPhoneAddressedChatIsLeftAlone(t *testing.T) {
	pn := phoneJID()
	if got := phoneAddressed(pn.String(), lidJID()); got != pn.String() {
		t.Fatalf("a phone chat must not be rewritten, got %q", got)
	}
}
