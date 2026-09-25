package hermesauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"ceraa/internal/secret"
)

func TestNousDeviceLoginStoresNativeCredential(t *testing.T) {
	secretRoot := t.TempDir()
	if err := secret.Init(filepath.Join(secretRoot, "key"), filepath.Join(secretRoot, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	var polls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/oauth/device/code":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "device-1", "user_code": "CODE-1", "verification_uri": "https://portal.example/device",
				"verification_uri_complete": "https://portal.example/device?code=CODE-1", "expires_in": 60, "interval": 1,
			})
		case "/api/oauth/token":
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-1", "refresh_token": "refresh-1", "expires_in": 900, "inference_base_url": "https://inference.example/v1"})
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
	if flow.UserCode != "CODE-1" || flow.VerificationURL == "" {
		t.Fatalf("flow = %+v", flow)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		status, _, _ := flow.StatusSnapshot()
		if status == "succeeded" {
			break
		}
		if status == "failed" {
			t.Fatalf("flow failed: %s", flow.ResultMessage())
		}
		time.Sleep(50 * time.Millisecond)
	}
	status, message, _ := flow.StatusSnapshot()
	if status != "succeeded" {
		t.Fatalf("status=%s message=%s", status, message)
	}
	credential, err := store.Resolve(context.Background(), "nous")
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "access-1" || credential.RefreshToken != "refresh-1" || credential.Source != "ceraa-native" {
		t.Fatalf("credential = %+v", credential)
	}
}
