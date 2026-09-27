package hermesauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ceraa/internal/secret"
)

// The WAF keys on the product token, not on a version, so the value is pinned to
// exactly what upstream sends when it cannot read its own version.
func TestClientUserAgentIsTheBareProductToken(t *testing.T) {
	if ClientUserAgent != "hermes-cli" {
		t.Errorf("ClientUserAgent = %q; the WAF keys on the hermes-cli product token", ClientUserAgent)
	}
}

func TestWithClientHeadersAddsTheIdentity(t *testing.T) {
	headers := WithClientHeaders(nil)
	if headers[HeaderUserAgent] != ClientUserAgent {
		t.Errorf("User-Agent = %q, want %q; the request would draw a Cloudflare 403", headers[HeaderUserAgent], ClientUserAgent)
	}
	if !HasClientHeaders(headers) {
		t.Error("HasClientHeaders did not recognise a set this package built")
	}
}

// A profile that pinned its own User-Agent is deliberate and must survive.
func TestWithClientHeadersKeepsADeliberateIdentity(t *testing.T) {
	original := map[string]string{HeaderUserAgent: "my-proxy/2.0", "X-Trace": "on"}
	headers := WithClientHeaders(original)
	if headers[HeaderUserAgent] != "my-proxy/2.0" {
		t.Errorf("User-Agent = %q; a profile's own identity must not be overridden", headers[HeaderUserAgent])
	}
	if headers["X-Trace"] != "on" {
		t.Error("an unrelated profile header was dropped")
	}
	if len(original) != 2 {
		t.Errorf("the input map was modified: %v", original)
	}
}

func TestWithClientHeadersDoesNotMutateItsInput(t *testing.T) {
	original := map[string]string{"X-Trace": "on"}
	_ = WithClientHeaders(original)
	if _, exists := original[HeaderUserAgent]; exists {
		t.Error("WithClientHeaders wrote into the caller's map")
	}
}

func TestHasClientHeadersRejectsAnEmptyValue(t *testing.T) {
	if HasClientHeaders(map[string]string{HeaderUserAgent: "   "}) {
		t.Error("a blank User-Agent was accepted as the client identity")
	}
	if HasClientHeaders(nil) {
		t.Error("an empty header set was accepted as the client identity")
	}
}

func TestRequestHeadersCarriesOnlyTheIdentity(t *testing.T) {
	headers := RequestHeaders()
	if headers[HeaderUserAgent] != ClientUserAgent {
		t.Errorf("RequestHeaders User-Agent = %q", headers[HeaderUserAgent])
	}
	if len(headers) != 1 {
		t.Errorf("RequestHeaders = %v; it should carry the identity alone", headers)
	}
}

// The authorization calls go to a host behind the same WAF, so they carry the
// identity too rather than depending on that host being less strict today.
func TestDeviceLoginSendsTheClientIdentity(t *testing.T) {
	secretRoot := t.TempDir()
	if err := secret.Init(filepath.Join(secretRoot, "key"), filepath.Join(secretRoot, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := map[string]string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get(HeaderUserAgent)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/oauth/device/code":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "device-1", "user_code": "CODE-1", "verification_uri": "https://portal.example/device",
				"expires_in": 60, "interval": 1,
			})
		case "/api/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "access-1", "refresh_token": "refresh-1", "expires_in": 900,
				"inference_base_url": "https://inference.example/v1",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	store := &Store{Home: t.TempDir(), PortalURL: server.URL, HTTPClient: server.Client(), RefreshSkew: time.Minute}
	manager := NewLoginManager(store)
	flow, err := manager.Start(context.Background(), "nous")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if status, _, _ := flow.StatusSnapshot(); status == "succeeded" || status == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/api/oauth/device/code", "/api/oauth/token"} {
		if seen[path] != ClientUserAgent {
			t.Errorf("%s User-Agent = %q, want %q", path, seen[path], ClientUserAgent)
		}
	}
}
