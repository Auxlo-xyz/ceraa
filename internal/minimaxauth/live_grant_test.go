package minimaxauth

import (
	"context"
	"os"
	"testing"
	"time"

	"ceraa/internal/oauthdevice"
)

// Read the live service only when asked: it touches a third party and creates a
// real, expiring user code.
func liveGrantDisabled() bool { return os.Getenv("CERAA_LIVE_GRANT") != "1" }

// The grant change is only observable here. Ceraa now asks as mcode-public, and
// the account host answers that grant with plain RFC 8628 numbers while it
// answered the CLI's grant with an absolute epoch and a millisecond interval.
// Nothing in the code would report a misreading: a login that cannot complete is
// indistinguishable from a user who walked away, so the expiry and the poll
// interval are checked against the live response.
func TestLiveGrantIsUnderstood(t *testing.T) {
	if liveGrantDisabled() {
		t.Skip("live grant check disabled")
	}
	login := NewLogin(RegionGlobal, nil)
	login.Manager().SetOnToken(func(string, oauthdevice.Token) error { return nil })
	flow, err := login.Start(context.Background())
	if err != nil {
		t.Fatalf("device code: %v", err)
	}
	snapshot := flow.Snapshot()
	t.Logf("client id : %s", OAuthClientID)
	t.Logf("scope     : %s", Scope)
	t.Logf("audience  : %s", Audience)
	t.Logf("verify    : %s", snapshot.VerificationURL)
	t.Logf("expires   : %s (in %s)", snapshot.ExpiresAt.Format(time.RFC3339), time.Until(snapshot.ExpiresAt).Round(time.Second))

	// expires_in: 300 read as an absolute epoch timestamp is 1970, and a flow born
	// expired cannot be approved. This is the check that would have caught the
	// dialect flags being carried over from the CLI grant.
	if d := time.Until(snapshot.ExpiresAt); d <= 0 || d > time.Hour {
		t.Errorf("the flow waits %s; expires_in was not read as a duration", d)
	}
	// The user has to be able to open the link the host gave us.
	if snapshot.VerificationURL == "" {
		t.Error("no verification URL")
	}
	if snapshot.UserCode == "" {
		t.Error("no user code")
	}
	login.Manager().Cancel(snapshot.ID)
}
