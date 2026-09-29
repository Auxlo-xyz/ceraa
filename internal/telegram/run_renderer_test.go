package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestEditTransportUsesOneFinalMessage(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	var finalMarkdown string
	var streamMarkdown string
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
				if markdown, ok := richMessage["markdown"].(string); ok {
					if strings.Contains(markdown, "▌") {
						mu.Lock()
						streamMarkdown = markdown
						mu.Unlock()
					}
					if markdown == "## Heading\n\n1. Step\n\nTelegram Message\n    ↓" {
						mu.Lock()
						finalMarkdown = markdown
						mu.Unlock()
					}
				}
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		case strings.HasSuffix(r.URL.Path, "/sendRichMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":14}}`))
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

	renderer := newRunRenderer("token", 42)
	defer renderer.close()
	answer := "## Heading\n\n1. Step\n\nTelegram Message\n    ↓"
	renderer.content("## Heading\n\n1. Step\n\nTelegram Message\n    ↓")
	renderer.finish(answer)
	renderer.close()

	mu.Lock()
	final := finalMarkdown
	stream := streamMarkdown
	paths := append([]string(nil), methods...)
	mu.Unlock()
	if final != answer {
		t.Fatalf("final message = %q", final)
	}
	if !strings.Contains(stream, "## Heading") || !strings.Contains(stream, "▌") {
		t.Fatalf("stream message = %q", stream)
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

func TestFinishDrainsPreviewBeforeFinalEdit(t *testing.T) {
	var mu sync.Mutex
	var edits []string
	previewStarted := make(chan struct{})
	releasePreview := make(chan struct{})
	var previewOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendRichMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":14}}`))
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if richMessage, ok := body["rich_message"].(map[string]any); ok {
				markdown, _ := richMessage["markdown"].(string)
				mu.Lock()
				edits = append(edits, markdown)
				mu.Unlock()
				if strings.Contains(markdown, "▌") {
					previewOnce.Do(func() { close(previewStarted) })
					<-releasePreview
				}
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

	renderer := newRunRenderer("token", 42)
	answer := "Complete answer"
	renderer.content("Complete")
	<-previewStarted
	finished := make(chan struct{})
	go func() {
		renderer.finish(answer)
		close(finished)
	}()
	select {
	case <-finished:
		t.Fatal("finish returned before the preview edit completed")
	default:
	}
	close(releasePreview)
	<-finished
	renderer.close()

	mu.Lock()
	defer mu.Unlock()
	if len(edits) < 2 || edits[len(edits)-1] != answer {
		t.Fatalf("final edit = %#v", edits)
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

	renderer := newRunRenderer("token", 42)
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

func TestThoughtStreamsInSeparateMessage(t *testing.T) {
	var mu sync.Mutex
	edits := make(map[int][]string)
	richSends := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/sendRichMessage") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			rich, _ := body["rich_message"].(map[string]any)
			markdown, _ := rich["markdown"].(string)
			mu.Lock()
			richSends++
			sendNumber := richSends
			mu.Unlock()
			messageID := 14
			if sendNumber > 1 {
				messageID = 16
			}
			_ = markdown
			_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, messageID)))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["parse_mode"] == "HTML" {
				_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":15}}`))
			} else {
				_, _ = w.Write([]byte(`{"ok":true}`))
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/editMessageText") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			id := int(body["message_id"].(float64))
			markdown, _ := body["text"].(string)
			if rich, ok := body["rich_message"].(map[string]any); ok {
				markdown, _ = rich["markdown"].(string)
			}
			mu.Lock()
			edits[id] = append(edits[id], markdown)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	oldBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = oldBase }()

	renderer := newRunRenderer("token", 42)
	renderer.reasoning("first step")
	renderer.reasoning("first step\nsecond step")
	renderer.finish("done")
	renderer.close()

	mu.Lock()
	thoughtEdits := append([]string(nil), edits[15]...)
	assistantEdits := append([]string(nil), edits[16]...)
	sendCount := richSends
	mu.Unlock()
	if sendCount < 2 {
		t.Fatalf("rich message sends = %d, want initial and replacement", sendCount)
	}
	if len(thoughtEdits) == 0 || !strings.Contains(thoughtEdits[0], "<blockquote expandable>") || !strings.Contains(thoughtEdits[0], "<b>Thought</b>") || !strings.Contains(thoughtEdits[0], "first step") {
		t.Fatalf("thought edits = %#v", thoughtEdits)
	}
	if !strings.Contains(thoughtEdits[len(thoughtEdits)-1], "second step") || strings.Contains(thoughtEdits[len(thoughtEdits)-1], "▌") {
		t.Fatalf("final thought edit = %#v", thoughtEdits)
	}
	if len(assistantEdits) == 0 || assistantEdits[len(assistantEdits)-1] != "done" {
		t.Fatalf("assistant edits = %#v", assistantEdits)
	}
	for _, edit := range assistantEdits {
		if strings.Contains(edit, "<blockquote expandable>") {
			t.Fatalf("thought leaked into assistant block: %#v", assistantEdits)
		}
	}
}

func TestToolActivityStaysOnAssistantMessage(t *testing.T) {
	var mu sync.Mutex
	var liveText string
	var finalMarkdown string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendRichMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":14}}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":12}}`))
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			if messageID, ok := body["message_id"].(float64); ok && messageID == 14 {
				if text, ok := body["text"].(string); ok {
					mu.Lock()
					liveText = text
					mu.Unlock()
				}
				if richMessage, ok := body["rich_message"].(map[string]any); ok {
					markdown, _ := richMessage["markdown"].(string)
					mu.Lock()
					if strings.Contains(markdown, "📚 Read file") {
						liveText = markdown
					} else {
						finalMarkdown = markdown
					}
					mu.Unlock()
				}
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

	renderer := newRunRenderer("token", 42)
	renderer.tool("read-1", "🔄 📚 Read file · /workspace/ceraa")
	renderer.tool("read-1", "✅ 📚 Read file completed")
	renderer.finish("Done")
	renderer.close()

	mu.Lock()
	live := liveText
	final := finalMarkdown
	mu.Unlock()
	if !strings.Contains(live, "📚 Read file") || !strings.Contains(live, "✓") || strings.Contains(live, "⏳") {
		t.Fatalf("live assistant message = %q", live)
	}
	if final != "Done" {
		t.Fatalf("final assistant message = %q", final)
	}
	// This used to assert that a pinned status message carried no tool activity.
	// There is no pinned message: PinnedManager was removed, having been
	// unreachable since newRunRenderer started discarding it, so the assertion had
	// nothing left to read and could no longer fail. Tool activity staying on the
	// assistant message is what the two assertions above cover.
}

func TestRunRendererStopCancelsTheRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	renderer := &runRenderer{}
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
	summary := toolArgsSummary("read", map[string]any{"args": `{"path":"/workspace/ceraa/.env"}`})
	if summary != " · /workspace/ceraa/.env" {
		t.Fatalf("tool argument summary = %q", summary)
	}
}

func TestToolActivityKeepsOneLinePerCallAndLastSix(t *testing.T) {
	renderer := &runRenderer{messageID: 1}
	for i := 1; i <= 7; i++ {
		callID := fmt.Sprintf("read-%d", i)
		renderer.tool(callID, fmt.Sprintf("🔄 📚 Read file · file-%d", i))
		renderer.tool(callID, "✅ 📚 Read file completed")
	}
	activity := formatToolActivity(renderer.tools)
	lines := strings.Split(activity, "\n")
	if len(lines) != 6 {
		t.Fatalf("tool lines = %d, want 6: %q", len(lines), activity)
	}
	if !strings.Contains(activity, "file-2  \n• ✓ 📚 Read file · file-3") {
		t.Fatalf("tool entries lack Markdown hard breaks: %q", activity)
	}
	if strings.Contains(activity, "file-1") || !strings.Contains(activity, "file-2") || !strings.Contains(activity, "file-7") {
		t.Fatalf("tool window = %q", activity)
	}
	if strings.Count(activity, "Read file") != 6 || strings.Contains(activity, "completed") {
		t.Fatalf("tool activity was not normalized: %q", activity)
	}
}

func TestToolActivityPreservesCommandDetailOnCompletion(t *testing.T) {
	renderer := &runRenderer{messageID: 1}
	renderer.tool("run-1", "🔄 >_ Run command · find /workspace -name config.json")
	renderer.tool("run-1", "✅ >_ Run command completed")
	want := "• ✓ >_ Run command · find /workspace -name config.json"
	if got := formatToolActivity(renderer.tools); got != want {
		t.Fatalf("tool activity = %q, want %q", got, want)
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
