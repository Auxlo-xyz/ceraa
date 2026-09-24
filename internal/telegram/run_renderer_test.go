package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestEditTransportUsesOneFinalMessage(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	var finalMarkdown string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if richMessage, ok := body["rich_message"].(map[string]any); ok {
				if markdown, ok := richMessage["markdown"].(string); ok && markdown == "## Heading\n\n1. Step\n\nTelegram Message\n    ↓" {
					mu.Lock()
					finalMarkdown = markdown
					mu.Unlock()
				}
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
	answer := "## Heading\n\n1. Step\n\nTelegram Message\n    ↓"
	renderer.content("## Heading\n\n1. Step\n\nTelegram Message\n    ↓")
	renderer.finish(answer)
	renderer.close()

	mu.Lock()
	final := finalMarkdown
	paths := append([]string(nil), methods...)
	mu.Unlock()
	if final != answer {
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

func TestRichMessageFallbackReplacesPreview(t *testing.T) {
	var mu sync.Mutex
	var sentMarkdown string
	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":12}}`))
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["rich_message"]; ok {
				_, _ = w.Write([]byte(`{"ok":false,"description":"rich edit is unavailable"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		case strings.HasSuffix(r.URL.Path, "/sendRichMessage"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if richMessage, ok := body["rich_message"].(map[string]any); ok {
				mu.Lock()
				sentMarkdown, _ = richMessage["markdown"].(string)
				mu.Unlock()
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":13}}`))
		case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
			mu.Lock()
			deleted = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
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
	answer := "## Heading\n\n1. Step"
	renderer.content(answer)
	renderer.finish(answer)
	renderer.close()

	mu.Lock()
	markdown := sentMarkdown
	wasDeleted := deleted
	mu.Unlock()
	if markdown != answer || !wasDeleted {
		t.Fatalf("rich fallback = %q, deleted=%t", markdown, wasDeleted)
	}
}

func TestToolActivityStaysOnAssistantMessage(t *testing.T) {
	var mu sync.Mutex
	var liveText string
	var finalMarkdown string
	var pinnedText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			messageID := 12
			if text, ok := body["text"].(string); ok {
				mu.Lock()
				pinnedText = text
				mu.Unlock()
				if text == "💬" {
					messageID = 13
				}
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":` + strconv.Itoa(messageID) + `}}`))
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			if messageID, ok := body["message_id"].(float64); ok && messageID == 13 {
				if text, ok := body["text"].(string); ok {
					mu.Lock()
					liveText = text
					mu.Unlock()
				}
			}
			if richMessage, ok := body["rich_message"].(map[string]any); ok {
				mu.Lock()
				finalMarkdown, _ = richMessage["markdown"].(string)
				mu.Unlock()
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
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
	renderer.tool("🔄 📚 Read file · /workspace/sooraa")
	renderer.tool("✅ 📚 Read file completed")
	renderer.finish("Done")
	renderer.close()

	mu.Lock()
	live := liveText
	final := finalMarkdown
	pinnedStatus := pinnedText
	mu.Unlock()
	if !strings.Contains(live, "🛠 Tools") || !strings.Contains(live, "📚 Read file") || strings.Contains(live, "⏳") {
		t.Fatalf("live assistant message = %q", live)
	}
	if final != "Done" {
		t.Fatalf("final assistant message = %q", final)
	}
	if strings.Contains(pinnedStatus, "🛠 Tools") {
		t.Fatalf("pinned message contains tools: %q", pinnedStatus)
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
	if got := formatToolStatus("🔄 📚 Read file · /workspace/sooraa"); got != "📚 Read file · /workspace/sooraa" {
		t.Fatalf("running tool status = %q", got)
	}
	if got := formatToolStatus("🔄 >_ Run command · ls"); got != ">_ Run command · ls" {
		t.Fatalf("running command status = %q", got)
	}
	if got := formatToolStatus("✅ Read file completed"); got != "✓ Read file completed" {
		t.Fatalf("completed tool status = %q", got)
	}
}

func TestToolDisplayNamesAreHumanReadable(t *testing.T) {
	if got := toolDisplayName("read"); got != "📚 Read file" {
		t.Fatalf("read label = %q", got)
	}
	if got := toolDisplayName("write"); got != "📝 Write file" {
		t.Fatalf("write label = %q", got)
	}
	if got := toolDisplayName("edit"); got != "📝 Edit file" {
		t.Fatalf("edit label = %q", got)
	}
	if got := toolDisplayName("bash"); got != ">_ Run command" {
		t.Fatalf("bash label = %q", got)
	}
}
