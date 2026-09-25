package hermesauth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveReadsActiveProviderPoolEntry(t *testing.T) {
	home := t.TempDir()
	writeAuthStore(t, home, `{"active_provider":"openrouter","credential_pool":{"openrouter":[{"id":"one","source":"manual","access_token":"secret","base_url":"https://example.test/v1","expires_at_ms":4102444800000}]}}`)
	store := &Store{Home: home, RefreshSkew: time.Minute}
	credential, err := store.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if credential.Provider != "openrouter" || credential.AccessToken != "secret" || credential.BaseURL != "https://example.test/v1" || credential.TokenType != "Bearer" {
		t.Fatalf("credential = %+v", credential)
	}
}

func TestResolveReadsProviderStateAndNormalizesCodex(t *testing.T) {
	home := t.TempDir()
	writeAuthStore(t, home, `{"providers":{"openai-codex":{"tokens":{"access_token":"codex-secret","refresh_token":"refresh"}}}}`)
	store := &Store{Home: home, RefreshSkew: time.Minute}
	credential, err := store.Resolve(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if credential.Provider != "openai-codex" || credential.AccessToken != "codex-secret" || credential.RefreshToken != "refresh" || credential.BaseURL != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("credential = %+v", credential)
	}
}

func TestResolveSkipsExhaustedEntries(t *testing.T) {
	home := t.TempDir()
	writeAuthStore(t, home, `{"credential_pool":{"openrouter":[{"access_token":"dead","last_status":"exhausted"},{"access_token":"live"}]}}`)
	store := &Store{Home: home}
	credential, err := store.Resolve(context.Background(), "openrouter")
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "live" {
		t.Fatalf("credential = %+v", credential)
	}
}

func writeAuthStore(t *testing.T, home, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
