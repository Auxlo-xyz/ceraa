package clineauth

import (
	"context"
	"os"
	"testing"
	"time"

	"ceraa/internal/oauthdevice"
)

func liveDisabled() bool { return os.Getenv("CERAA_LIVE_OAUTH") != "1" }

// Drives the real Cline/WorkOS device flow. Skipped by default because it
// touches a third party and creates a real, short-lived user code.
func TestLiveDeviceLogin(t *testing.T) {
	if liveDisabled() {
		t.Skip("live OAuth disabled")
	}
	login := NewLogin(nil)
	login.Manager().SetOnToken(func(string, oauthdevice.Token) error { return nil })
	flow, err := login.Start(context.Background())
	if err != nil {
		t.Fatalf("device code: %v", err)
	}
	snapshot := flow.Snapshot()
	t.Logf("user_code : %s", snapshot.UserCode)
	t.Logf("verify    : %s", snapshot.VerificationURL)
	t.Logf("waits     : %s", time.Until(snapshot.ExpiresAt).Round(time.Second))
	if snapshot.UserCode == "" || snapshot.VerificationURL == "" {
		t.Fatal("the login produced a code with nowhere to enter it")
	}
	// Nothing is approved, so the flow must stay pending rather than reporting a
	// token. This is the case a browser-tab bridge cannot express at all.
	time.Sleep(2 * time.Second)
	if got := flow.Snapshot().Status; got != oauthdevice.StatusPending {
		t.Errorf("status = %q, want pending", got)
	}
	_ = login.Manager().Cancel(flow.ID)
}

// The catalog needs no credential, which is how a user sees what signing in gets
// them. A gateway that required auth here would make that impossible.
func TestLiveCatalog(t *testing.T) {
	if liveDisabled() {
		t.Skip("live OAuth disabled")
	}
	catalog, err := FetchCatalog(context.Background(), nil)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	t.Logf("free=%d recommended=%d clinePass=%d clineCloud=%d",
		len(catalog.Free), len(catalog.Recommended), len(catalog.ClinePass), len(catalog.ClineCloud))
	if len(catalog.Free) == 0 {
		t.Fatal("the free tier is empty; the gateway changed shape")
	}
	for _, m := range catalog.Free {
		t.Logf("  free: %-34s %s", m.ID, m.Name)
	}
}
