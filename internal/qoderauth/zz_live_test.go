package qoderauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ceraa/internal/secret"
)

// TestLiveQoderDeviceAuth is a manual probe against the real endpoint. It is
// skipped unless CERAA_LIVE_QODER=1, because it needs a human to approve and it
// creates a real authorization on the account.
//
// It exercises the production Manager, so what is proven is the code Ceraa runs:
// the PKCE construction, the approval URL shape, the 404-as-pending poll, and
// the token storage. The tail of the test then proves the stored token is valid
// for API use by fetching the account profile, which is precisely what the
// inference client will need.
func TestLiveQoderDeviceAuth(t *testing.T) {
	if os.Getenv("CERAA_LIVE_QODER") != "1" {
		t.Skip("set CERAA_LIVE_QODER=1 to run the live device login")
	}
	// CERAA_PROBE_HOME overrides the temp store, so a token obtained for
	// probing survives the test process and can be reused for inference probes
	// without another approval. Unset, the default is a temp dir that vanishes,
	// so a real token never lands in the working tree by accident.
	dir := os.Getenv("CERAA_PROBE_HOME")
	if dir == "" {
		dir = t.TempDir()
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("probe home: %v", err)
	}
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}

	manager := NewManager() // production endpoints
	flow, err := manager.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Logf("ACTION REQUIRED: open this URL in a browser and approve:")
	t.Logf("  %s", flow.Snapshot().AuthURL)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if err := manager.Poll(ctx, flow); err != nil {
		t.Fatalf("poll: %v", err)
	}
	t.Logf("poll accepted the token")

	if !Authenticated() {
		t.Fatal("the token was stored but Authenticated() is false")
	}
	t.Logf("Authenticated() is true")

	token, err := secret.Get(keyAccessToken)
	if err != nil || token == "" {
		t.Fatalf("no access token in the store: %v", err)
	}
	if _, err := secret.Get(keyRefreshToken); err != nil {
		t.Logf("no refresh token stored (the integration must survive this, failing loudly at expiry)")
	} else {
		t.Logf("a refresh token was stored")
	}

	profile, err := fetchUserInfo(ctx, token)
	if err != nil {
		t.Fatalf("the stored token was rejected by userinfo: %v", err)
	}
	t.Logf("userinfo accepted the token: %s", profile)
}

// fetchUserInfo proves the token is valid for API use. It is the same proof the
// inference client will need, and a token that authenticates a login but cannot
// call the API is not a token this integration can use.
func fetchUserInfo(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		DefaultEndpoints().userInfoURL(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("userinfo unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("userinfo status %d: %s", resp.StatusCode, firstLine(body))
	}
	// Report shape, never value. The point is that a profile came back, not what
	// it says.
	var shape map[string]any
	if err := json.Unmarshal(body, &shape); err != nil {
		return fmt.Sprintf("%d bytes, not JSON", len(body)), nil
	}
	keys := make([]string, 0, len(shape))
	for k := range shape {
		keys = append(keys, k)
	}
	return fmt.Sprintf("%d bytes, keys %v", len(body), keys), nil
}
