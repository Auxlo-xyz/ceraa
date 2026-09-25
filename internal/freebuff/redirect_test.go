package freebuff

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSecureClientRefusesCrossHostRedirect pins that a credentialed request is
// never followed off-site. Following it would hand the bearer token to whatever
// host the redirect names.
func TestSecureClientRefusesCrossHostRedirect(t *testing.T) {
	var leaked string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "u"})
	}))
	defer attacker.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/api/v1/me", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client := NewClient()
	client.APIBaseURL = origin.URL
	client.HTTPClient = newSecureClient()
	if _, err := client.Me(context.Background(), "secret-token"); err == nil {
		t.Fatal("expected the cross-host redirect to be refused")
	} else if !strings.Contains(err.Error(), "refusing to follow redirect") {
		t.Fatalf("error = %v, want an explicit refusal", err)
	}
	if leaked != "" {
		t.Fatalf("Authorization reached the redirect target: %q", leaked)
	}
}

// TestSecureClientKeepsCredentialOnSameHostRedirect pins the reason the API base
// is the canonical www host: a same-site redirect must not silently drop the
// token, which would read as a revoked credential.
func TestSecureClientKeepsCredentialOnSameHostRedirect(t *testing.T) {
	var mux *http.ServeMux
	server := httptest.NewServer(nil)
	defer server.Close()
	mux = http.NewServeMux()
	mux.HandleFunc("/api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "unauthorized"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "u", "email": "dev@example.com"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/v1/me", http.StatusMovedPermanently)
	})
	server.Config.Handler = mux

	client := NewClient()
	client.APIBaseURL = server.URL
	client.HTTPClient = newSecureClient()
	user, err := client.Me(context.Background(), "secret-token")
	if err != nil {
		t.Fatalf("same-host redirect dropped the credential: %v", err)
	}
	if user.Email != "dev@example.com" {
		t.Fatalf("user = %+v", user)
	}
}

// TestDefaultAPIBaseDoesNotRedirect guards the reason the www host is baked in:
// the bare host 301s, and the redirect is what would strip the token.
func TestDefaultAPIBaseDoesNotRedirect(t *testing.T) {
	if !strings.HasPrefix(DefaultAPIBaseURL, "https://") {
		t.Fatalf("the API base must be https, got %q", DefaultAPIBaseURL)
	}
	if strings.Contains(strings.TrimPrefix(DefaultAPIBaseURL, "https://"), "/") {
		t.Fatalf("the API base must be a bare host, got %q", DefaultAPIBaseURL)
	}
}
