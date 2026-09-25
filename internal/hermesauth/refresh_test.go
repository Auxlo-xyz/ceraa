package hermesauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRefreshNousPortalUsesDirectTokenEndpoint(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/token" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("x-nous-refresh-token") != "old-refresh" {
			t.Fatalf("refresh header = %q", r.Header.Get("x-nous-refresh-token"))
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != defaultNousClientID {
			t.Fatalf("form = %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-invoke-jwt", "refresh_token": "rotated-refresh", "expires_in": 900, "inference_base_url": "https://inference.example/v1"})
	}))
	defer server.Close()
	store := &Store{HTTPClient: server.Client()}
	credential, err := store.refreshCredential(context.Background(), Credential{Provider: "nous", PortalBaseURL: server.URL, ClientID: defaultNousClientID, RefreshToken: "old-refresh", BaseURL: "https://old.example/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "new-invoke-jwt" || credential.RefreshToken != "rotated-refresh" || credential.BaseURL != "https://inference.example/v1" {
		t.Fatalf("credential = %+v", credential)
	}
}

func TestResolveRefreshesQwenWithoutHermesCLI(t *testing.T) {
	home := t.TempDir()
	refreshServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/token" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("client_id") != qwenClientID {
			t.Fatalf("form = %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600})
	}))
	defer refreshServer.Close()
	state := map[string]any{
		"providers": map[string]any{
			"qwen-oauth": map[string]any{
				"access_token":   "old-access",
				"refresh_token":  "old-refresh",
				"expires_at":     time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
				"token_endpoint": refreshServer.URL + "/token",
			},
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	store := &Store{Home: home, RefreshSkew: time.Minute, HTTPClient: refreshServer.Client()}
	credential, err := store.Resolve(context.Background(), "qwen-oauth")
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "new-access" || credential.RefreshToken != "new-refresh" {
		t.Fatalf("credential = %+v", credential)
	}
	updated, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(updated, &saved); err != nil {
		t.Fatal(err)
	}
	provider := saved["providers"].(map[string]any)["qwen-oauth"].(map[string]any)
	if provider["access_token"] != "new-access" || provider["refresh_token"] != "new-refresh" {
		t.Fatalf("persisted provider = %#v", provider)
	}
}
