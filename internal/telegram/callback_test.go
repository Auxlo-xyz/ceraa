package telegram

import (
	"encoding/json"
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

func TestSendMenuMarkupTracksTrigger(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 7}})
	}))
	defer server.Close()
	oldBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = oldBase }()

	setMenuTriggerPending(42, 5)
	sendMenuMarkup("token", 42, "Menu", nil)
	if got := menuTriggerForMessage(42, 7); got != 5 {
		t.Fatalf("menu trigger = %d", got)
	}
	menuTriggerByMessage.Delete(menuTriggerKey(42, 7))
	menuTriggerPending.Delete(42)
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

func TestCloseCallbackMenuDeletesMenuAndTrigger(t *testing.T) {
	var mu sync.Mutex
	var deleted []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MessageID int `json:"message_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		deleted = append(deleted, body.MessageID)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	oldBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = oldBase }()

	menuTriggerByMessage.Store(menuTriggerKey(42, 7), 5)
	closeCallbackMenu("token", 42, 7)

	mu.Lock()
	got := append([]int(nil), deleted...)
	mu.Unlock()
	if len(got) != 2 || got[0] != 7 || got[1] != 5 {
		t.Fatalf("deleted message IDs = %v", got)
	}
	if menuTriggerForMessage(42, 7) != 0 {
		t.Fatal("menu trigger mapping was not cleared")
	}
}
