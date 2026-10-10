package traeauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/internal/secret"
)

func isolatedStore(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
}

// The exchange contract is exact: RefreshToken in, Data.Token out, and a
// rejection surfaces the vendor code rather than a bare HTTP status.
func TestExchangeTradesPATForToken(t *testing.T) {
	isolatedStore(t)
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cloudide/api/v3/trae/oauth/ExchangeToken" {
			t.Errorf("exchange path = %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"Data":{"Token":"ide-abc","TokenExpireAt":9999999999999}}`))
	}))
	defer srv.Close()
	SetBaseURL(srv.URL)
	defer SetBaseURL("https://api.trae.ai")

	token, expiresAt, err := exchange(context.Background(), srv.URL, "pat-123")
	if err != nil {
		t.Fatal(err)
	}
	if token != "ide-abc" || expiresAt != 9999999999999 {
		t.Errorf("exchange = %q %d", token, expiresAt)
	}
	if gotBody["RefreshToken"] != "pat-123" {
		t.Errorf("exchange body = %v", gotBody)
	}
}

func TestExchangeRejectsBadPAT(t *testing.T) {
	isolatedStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":4001,"message":"invalid token"}`))
	}))
	defer srv.Close()

	if _, _, err := exchange(context.Background(), srv.URL, "bogus"); err == nil || !strings.Contains(err.Error(), "4001") {
		t.Errorf("expected code 4001 error, got %v", err)
	}
}

// SavePAT stores only after a successful exchange, and AccessToken reuses the
// cached token until it nears expiry.
func TestSavePATAndCachedAccessToken(t *testing.T) {
	isolatedStore(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"Data":{"Token":"ide-live","TokenExpireAt":9999999999999}}`))
	}))
	defer srv.Close()
	SetBaseURL(srv.URL)
	defer SetBaseURL("https://api.trae.ai")

	if err := SavePAT(context.Background(), "pat-xyz"); err != nil {
		t.Fatal(err)
	}
	if !Authenticated() {
		t.Error("Authenticated = false after SavePAT")
	}
	got, err := AccessToken(context.Background())
	if err != nil || got != "ide-live" {
		t.Errorf("AccessToken = %q %v", got, err)
	}
	if calls != 1 {
		t.Errorf("exchange calls = %d, want 1 (cached token reused)", calls)
	}
}

func TestRequestHeadersCarryClientIdentity(t *testing.T) {
	h := RequestHeaders("tok")
	if h["Authorization"] != "Cloud-IDE-JWT tok" {
		t.Errorf("Authorization = %q", h["Authorization"])
	}
	if h["X-App-Id"] == "" || h["X-Ide-Version-Code"] == "" || h["X-Ide-Function"] != "chat" {
		t.Errorf("identity headers = %v", h)
	}
}
