package minimaxauth

import (
	"context"
	"os"
	"testing"
	"time"

	"ceraa/internal/oauthdevice"
)

func liveDisabled() bool { return os.Getenv("CERAA_LIVE_OAUTH") != "1" }

// Reads the live MiniMax service. Skipped by default because it touches a third
// party and creates a real, expiring user code.
func TestLiveDeviceLogin(t *testing.T) {
	if liveDisabled() {
		t.Skip("live OAuth disabled")
	}
	login := NewLogin(RegionGlobal, nil)
	login.Manager().SetOnToken(func(string, oauthdevice.Token) error { return nil })
	flow, err := login.Start(context.Background())
	if err != nil {
		t.Fatalf("device code: %v", err)
	}
	snapshot := flow.Snapshot()
	t.Logf("region    : %s", login.Region())
	t.Logf("user_code : %s", snapshot.UserCode)
	t.Logf("verify    : %s", snapshot.VerificationURL)
	t.Logf("expires   : %s", snapshot.ExpiresAt.Format(time.RFC3339))
	t.Logf("waits     : %s", time.Until(snapshot.ExpiresAt).Round(time.Second))
	if snapshot.UserCode == "" {
		t.Fatal("no user code")
	}
	// The absolute epoch-ms expiry must land about now, not in the year 58709.
	if d := time.Until(snapshot.ExpiresAt); d <= 0 || d > time.Hour {
		t.Errorf("the flow waits %s, want the absolute expiry to be near", d)
	}
	time.Sleep(2 * time.Second)
	if got := flow.Snapshot().Status; got != oauthdevice.StatusPending {
		t.Errorf("status = %q after 2s, want pending; a 200 pending body was read as a token", got)
	}
	_ = login.Manager().Cancel(flow.ID)
}
