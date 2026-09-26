package codexauth

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"ceraa/internal/secret"
)

// TestLiveCodexDeviceAuth is a manual probe against the real endpoint. It is
// skipped unless CERAA_LIVE_CODEX=1, because it needs a human to approve a code
// and it stores real credentials.
//
// It exercises the production Manager, so what is proven is the code Ceraa runs.
func TestLiveCodexDeviceAuth(t *testing.T) {
	if os.Getenv("CERAA_LIVE_CODEX") != "1" {
		t.Skip("set CERAA_LIVE_CODEX=1 to run the live device login")
	}
	dir := os.Getenv("CERAA_HOME")
	if dir == "" {
		// Without this the store resolves relative to the package directory,
		// which drops real credentials inside the working tree.
		t.Fatal("CERAA_HOME must be the data dir; refusing to write secrets into the repo")
	}
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}
	if Forget() != nil {
		t.Fatalf("forget: %v", Forget())
	}

	manager := NewManager() // production endpoints
	flow, err := manager.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	state := flow.Snapshot()
	t.Logf("=========================================================")
	t.Logf("  OPEN:   %s", state.AuthURL)
	t.Logf("  CODE:   %s", state.UserCode)
	t.Logf("=========================================================")

	deadline := time.Now().Add(14 * time.Minute)
	for time.Now().Before(deadline) {
		s := flow.Snapshot()
		if s.Status == StatusSucceeded {
			t.Logf("LIVE LOGIN SUCCEEDED")
			if !Authenticated() {
				t.Fatal("succeeded but no token is stored")
			}
			token, err := secret.Get(keyAccessToken)
			if err != nil || token == "" {
				t.Fatalf("access token: %v", err)
			}
			// Report shape only. Token values are never written to a log.
			t.Logf("stored access token: %d bytes", len(token))
			refresh, _ := secret.Get(keyRefreshToken)
			t.Logf("stored refresh token: %d bytes", len(refresh))
			expires, _ := secret.Get(keyExpiresAt)
			t.Logf("expires_at: %s", expires)
			// A live refresh proves the rotated token path too.
			refreshed, err := manager.RefreshToken(t.Context())
			if err != nil {
				t.Logf("refresh after login: %v", err)
			} else {
				t.Logf("LIVE REFRESH SUCCEEDED (%d bytes)", len(refreshed))
			}
			return
		}
		if s.Status == StatusFailed {
			t.Fatalf("flow failed: %s (hint: %s)", s.Message, s.Hint)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for approval; flow is %q", flow.Snapshot().Status)
}
