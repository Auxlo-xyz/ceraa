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

func TestEditTransportUsesOneFinalMessage(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	var finalText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if text, ok := body["text"].(string); ok && text == "Final answer" {
				mu.Lock()
				finalText = text
				mu.Unlock()
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
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
	paths := append([]string(nil), methods...)
	mu.Unlock()
	if final != "Final answer" {
		t.Fatalf("final message = %q", final)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "/sendMessageDraft") {
			t.Fatalf("native draft transport was used: %v", paths)
		}
	}
	edits := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "/editMessageText") {
			edits++
		}
	}
	if edits < 2 {
		t.Fatalf("expected streamed and final edits, got %v", paths)
	}
}

func TestRunRendererStopCancelsTheRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pinned := NewPinnedManager("token", 42)
	defer pinned.close()
	renderer := &runRenderer{pinned: pinned}
	renderer.setCancel(cancel)
	renderer.stop()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("stopped run did not cancel")
	}
	if !renderer.isStopped() {
		t.Fatal("renderer did not record the stop")
	}
}

func TestToolActivityIsReadableAndRedacted(t *testing.T) {
	summary := toolArgsSummary("read", map[string]any{"args": `{"path":"/workspace/sooraa/.env"}`})
	if summary != " · /workspace/sooraa/.env" {
		t.Fatalf("tool argument summary = %q", summary)
	}
	if got := formatToolStatus("🔄 Read file · /workspace/sooraa"); got != "⏳ Read file · /workspace/sooraa" {
		t.Fatalf("running tool status = %q", got)
	}
	if got := formatToolStatus("✅ Read file completed"); got != "✓ Read file completed" {
		t.Fatalf("completed tool status = %q", got)
	}
}

func TestToolDisplayNamesAreHumanReadable(t *testing.T) {
	if got := toolDisplayName("read"); got != "Read file" {
		t.Fatalf("read label = %q", got)
	}
	if got := toolDisplayName("bash"); got != "Run command" {
		t.Fatalf("bash label = %q", got)
	}
}
