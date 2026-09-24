package telegram

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRemoveCallbackMarkupFallsBackToEditText(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/editMessageReplyMarkup") {
			_, _ = w.Write([]byte(`{"ok":false,"description":"empty keyboard rejected"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	oldBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = oldBase }()

	removeCallbackMarkup("token", 42, 7, "Menu text")

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || !strings.HasSuffix(calls[0], "/editMessageReplyMarkup") || !strings.HasSuffix(calls[1], "/editMessageText") {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestRemoveCallbackMarkupAcceptsEmptyKeyboard(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	oldBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = oldBase }()

	removeCallbackMarkup("token", 42, 7, "Menu text")
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}
