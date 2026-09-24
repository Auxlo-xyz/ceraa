package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestSendMessageDraftUsesStableDraftID(t *testing.T) {
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode draft: %v", err)
		}
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	oldBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = oldBase }()

	if err := sendMessageDraft("token", 42, 12345, "hello", true); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("draft requests = %d", len(requests))
	}
	request := requests[0]
	if request["draft_id"].(float64) != 12345 || request["chat_id"].(float64) != 42 || request["text"].(string) != "hello" || request["can_stop"] != true {
		t.Fatalf("unexpected draft request: %+v", request)
	}
}

func TestRunRendererPersistsFinalAnswer(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	var draftIDs []int64
	var finalText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessageDraft"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if draftID, ok := body["draft_id"].(float64); ok {
				mu.Lock()
				draftIDs = append(draftIDs, int64(draftID))
				mu.Unlock()
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if text, ok := body["text"].(string); ok && text == "Final answer" {
				mu.Lock()
				finalText = text
				mu.Unlock()
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":12}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer server.Close()
	oldBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = oldBase }()

	pinned := NewPinnedManager("token", 42)
	defer pinned.close()
	pinned.Init()
	renderer := newRunRenderer("token", 42, pinned)
	defer renderer.close()
	renderer.content("Final ")
	renderer.content("answer")
	renderer.finish("Final answer")
	renderer.close()

	mu.Lock()
	final := finalText
	mu.Unlock()
	if final != "Final answer" {
		t.Fatalf("final message = %q", final)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) < 3 {
		t.Fatalf("expected draft, edit, and final methods, got %v", methods)
	}
	if len(draftIDs) < 2 {
		t.Fatalf("expected initial and content drafts, got %v", draftIDs)
	}
	for _, draftID := range draftIDs[1:] {
		if draftID != draftIDs[0] {
			t.Fatalf("draft ID changed during stream: %v", draftIDs)
		}
	}
}

func TestUpdateDecodesStoppedMessageGeneration(t *testing.T) {
	var value update
	if err := json.Unmarshal([]byte(`{"update_id":1,"stopped_message_generation":{"chat":{"id":42},"draft_id":9876}}`), &value); err != nil {
		t.Fatal(err)
	}
	if value.StoppedMessageGeneration == nil || value.StoppedMessageGeneration.Chat.ID != 42 || value.StoppedMessageGeneration.DraftID != 9876 {
		t.Fatalf("unexpected stop update: %+v", value.StoppedMessageGeneration)
	}
}

func TestRunRendererStopCancelsTheRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pinned := NewPinnedManager("token", 42)
	defer pinned.close()
	renderer := &runRenderer{pinned: pinned, draftID: 9876}
	activeDrafts.Store(renderer.draftID, renderer)
	renderer.setCancel(cancel)
	renderer.stop()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("stopped draft did not cancel the run")
	}
	if !renderer.isStopped() {
		t.Fatal("renderer did not record the stop")
	}
	renderer.close()
}

func TestToolDisplayNamesAreHumanReadable(t *testing.T) {
	if got := toolDisplayName("read"); got != "Read file" {
		t.Fatalf("read label = %q", got)
	}
	if got := toolDisplayName("bash"); got != "Run command" {
		t.Fatalf("bash label = %q", got)
	}
}
