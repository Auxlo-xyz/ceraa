package sessionnamer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ceraa/harness/sessionstore"
)

func TestDefaultConfigUsesGeminiFlashLite(t *testing.T) {
	t.Setenv("SOORAA_TITLE_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("SOORAA_TITLE_API_URL", "")
	t.Setenv("SOORAA_TITLE_MODEL", "")
	cfg := DefaultConfig()
	if cfg.APIURL != DefaultAPIURL || cfg.Model != DefaultModel {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestGenerateCleansGeminiTitle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"\\\"API migration.\\\"\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	sess := &sessionstore.Session{Messages: []sessionstore.Message{
		{Role: "user", Content: "Migrate the API"},
		{Role: "assistant", Content: "I will inspect the project."},
	}}
	title, err := Generate(context.Background(), Config{APIURL: server.URL, APIKey: "test", Model: "test-model"}, sess)
	if err != nil {
		t.Fatal(err)
	}
	if title != "API migration" {
		t.Fatalf("title = %q", title)
	}
}

func TestHasMeaningfulPrompt(t *testing.T) {
	if HasMeaningfulPrompt(&sessionstore.Session{Messages: []sessionstore.Message{{Role: "user", Content: "hi"}}}) {
		t.Fatal("short greeting should not trigger title generation")
	}
	if !HasMeaningfulPrompt(&sessionstore.Session{Messages: []sessionstore.Message{{Role: "user", Content: "Migrate the API"}}}) {
		t.Fatal("task prompt should trigger title generation")
	}
}

func TestFallbackUsesFirstUserMessage(t *testing.T) {
	sess := &sessionstore.Session{Messages: []sessionstore.Message{{Role: "user", Content: "  Fix\n  the login bug  "}}}
	if got := Fallback(sess); got != "Fix the login bug" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestGenerateRequiresAPIKey(t *testing.T) {
	_, err := Generate(context.Background(), Config{}, &sessionstore.Session{Messages: []sessionstore.Message{{Role: "user", Content: "hello"}}})
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("error = %v", err)
	}
}
