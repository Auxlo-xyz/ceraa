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
	"time"
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
	posted := make([]string, 0)
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
			// One assistant message, id 14, for the whole run. The stub still
			// hands out a second id so a reintroduced replacement shows up as an
			// extra send rather than as a silently reused id.
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
				text, _ := body["text"].(string)
				mu.Lock()
				posted = append(posted, text)
				mu.Unlock()
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
	renderer.reasoning("\nsecond step")
	renderer.finish("done")
	renderer.close()

	mu.Lock()
	thoughtEdits := append([]string(nil), edits[15]...)
	initialThought := append([]string(nil), posted...)
	assistantEdits := append([]string(nil), edits[14]...)
	sendCount := richSends
	mu.Unlock()
	// One assistant message for the whole run. A thought used to replace it, which
	// left a bare cursor bubble behind whenever the delete failed, and that bubble
	// was the only thing between the reader and the answer.
	if sendCount != 1 {
		t.Fatalf("rich message sends = %d, want exactly one assistant message", sendCount)
	}
	// The block opens as a plain blockquote, which is the shape that can be
	// redrawn without anything to snap shut.
	if len(initialThought) == 0 {
		t.Fatal("the thought was never posted")
	}
	if !strings.HasPrefix(initialThought[0], "<blockquote>"+thoughtHeader) || strings.Contains(initialThought[0], "expandable") {
		t.Fatalf("streaming thought = %q, want a plain blockquote", initialThought[0])
	}
	// One redraw for the whole run: the published thought, written once. The
	// intermediate state was paced out by the thought's own floor rather than
	// redrawn on every signal.
	if len(thoughtEdits) != 1 {
		t.Fatalf("thought redraws = %#v, want only the published one", thoughtEdits)
	}
	if !strings.Contains(thoughtEdits[0], "<blockquote expandable>") || !strings.Contains(thoughtEdits[0], thoughtHeader) || !strings.Contains(thoughtEdits[0], "first step") {
		t.Fatalf("published thought = %#v", thoughtEdits[0])
	}
	if !strings.Contains(thoughtEdits[0], "first step\nsecond step") || strings.Contains(thoughtEdits[0], "▌") {
		t.Fatalf("published thought = %#v", thoughtEdits[0])
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

// The tool trail is a message of its own, not a block in front of the answer.
// Splitting them is what lets a running line be redrawn without re-PUTting the
// answer, and what lets the published answer stand on its own once the run is
// over while the trail stays below it as the record of what happened.
func TestToolActivityGetsItsOwnMessage(t *testing.T) {
	var mu sync.Mutex
	byID := make(map[int]string)
	sends := 0
	answerID := 0
	trailID := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodyText := func() (int, string) {
			id := 0
			if n, ok := body["message_id"].(float64); ok {
				id = int(n)
			}
			text, _ := body["text"].(string)
			if richMessage, ok := body["rich_message"].(map[string]any); ok {
				text, _ = richMessage["markdown"].(string)
			}
			return id, text
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendRichMessage"):
			_, text := bodyText()
			mu.Lock()
			sends++
			id := 14
			if sends > 1 {
				id = 15
				trailID = id
			} else {
				answerID = id
			}
			byID[id] = text
			mu.Unlock()
			_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, id)))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":12}}`))
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			id, text := bodyText()
			mu.Lock()
			if id != 0 {
				byID[id] = text
			}
			mu.Unlock()
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
	renderer.tool("read-1", "🔄 "+toolNameToken("read")+"📚 Read file · /workspace/ceraa")
	renderer.tool("read-1", "✅ "+toolNameToken("read")+"📚 Read file completed")
	renderer.finish("Done")
	renderer.close()

	mu.Lock()
	answer := byID[answerID]
	trail := byID[trailID]
	ids := []int{}
	for id := range byID {
		ids = append(ids, id)
	}
	mu.Unlock()
	if trailID == 0 || trail == "" {
		t.Fatalf("the tool trail never got its own message: ids=%v answer=%q", ids, answer)
	}
	if !strings.Contains(trail, "📖 read /workspace/ceraa") {
		t.Fatalf("trail message = %q", trail)
	}
	if strings.Contains(trail, "\u23f3") {
		t.Fatalf("trail message carries a running marker after completion: %q", trail)
	}
	if strings.Contains(answer, "📚 Read file") {
		t.Fatalf("tool activity leaked into the answer message: %q", answer)
	}
	if answer != "Done" {
		t.Fatalf("final answer = %q", answer)
	}
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

// Every call keeps its own line and none are dropped. The old six-line window
// is gone: it existed because the lines were bullets sharing one paragraph, and
// a window over a list that grows for twenty minutes reads as the run forgetting
// what it did.
func TestToolActivityKeepsOneLinePerCallAndEveryCall(t *testing.T) {
	renderer := &runRenderer{messageID: 1}
	for i := 1; i <= 7; i++ {
		callID := fmt.Sprintf("read-%d", i)
		renderer.tool(callID, fmt.Sprintf("🔄 %sread %s file-%d", toolNameToken("read"), "📚 Read file · ", i))
		renderer.tool(callID, "✅ "+toolNameToken("read")+"📚 Read file completed")
	}
	activity := formatToolActivity(renderer.tools)
	lines := strings.Split(activity, "\n\n")
	if len(lines) != 7 {
		t.Fatalf("tool lines = %d, want 7: %q", len(lines), activity)
	}
	for _, i := range []int{1, 2, 7} {
		if !strings.Contains(activity, fmt.Sprintf("file-%d", i)) {
			t.Fatalf("call %d is missing from the trail: %q", i, activity)
		}
	}
	// The icon and the bare tool name are the line, so a reader can tell what
	// ran without reading the harness's sentence about it.
	if !strings.Contains(activity, "📖 read file-1") {
		t.Fatalf("tool line is not icon plus tool name: %q", activity)
	}
	if strings.Contains(activity, "completed") {
		t.Fatalf("tool activity was not normalized: %q", activity)
	}
}

func TestToolActivityPreservesCommandDetailOnCompletion(t *testing.T) {
	renderer := &runRenderer{messageID: 1}
	renderer.tool("run-1", "🔄 "+toolNameToken("bash")+">_ Run command · find /workspace -name config.json")
	renderer.tool("run-1", "✅ "+toolNameToken("bash")+">_ Run command completed")
	want := "💻 bash find /workspace -name config.json"
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

// A flat floor paced a twenty minute run the same as a two second one, which is
// how the recorded incident ended in a 429 with retry after 227s. The ladder
// widens with the age of the run, and no tier is ever below the floor the
// thought is pinned to.
func TestStreamThrottleWidensWithTheAgeOfTheRun(t *testing.T) {
	cases := []struct {
		age  time.Duration
		want time.Duration
	}{
		{0, time.Second},
		{time.Minute - time.Second, time.Second},
		{time.Minute, 2 * time.Second},
		{5*time.Minute - time.Second, 2 * time.Second},
		{5 * time.Minute, 5 * time.Second},
		{15*time.Minute - time.Second, 5 * time.Second},
		{15 * time.Minute, 10 * time.Second},
		{time.Hour, 10 * time.Second},
	}
	for _, tc := range cases {
		got := streamThrottle(tc.age)
		if got != tc.want {
			t.Errorf("streamThrottle(%s) = %s, want %s", tc.age, got, tc.want)
		}
		if got < thoughtMinInterval {
			t.Errorf("streamThrottle(%s) = %s, below the thought floor %s", tc.age, got, thoughtMinInterval)
		}
	}
}

// A running line is one line that moves, not one row per progress report.
// Appending made a busy run unreadable and pushed the answer off the message.
func TestRunningToolKeepsOneLinePerCall(t *testing.T) {
	renderer := &runRenderer{messageID: 1}
	for i := 0; i < 5; i++ {
		renderer.tool("run-1", fmt.Sprintf("🔄 >_ Run command · find /x -name f%d", i))
	}
	if len(renderer.tools) != 1 {
		t.Fatalf("running lines = %d, want 1 for one call: %q", len(renderer.tools), formatToolActivity(renderer.tools))
	}
	if !strings.Contains(formatToolActivity(renderer.tools), "f4") {
		t.Fatalf("the newest progress report did not replace the line: %q", formatToolActivity(renderer.tools))
	}
	// Two calls that share a label are still two calls: a running line must not
	// be merged by label, or one of them would silently vanish.
	renderer.tool("run-2", "🔄 >_ Run command · find /x -name other")
	if len(renderer.tools) != 2 {
		t.Fatalf("running lines = %d, want 2 distinct calls: %q", len(renderer.tools), formatToolActivity(renderer.tools))
	}
	renderer.tool("run-1", "✅ >_ Run command completed")
	if len(renderer.tools) != 2 {
		t.Fatalf("completion grew the list to %d: %q", len(renderer.tools), formatToolActivity(renderer.tools))
	}
	if !strings.Contains(formatToolActivity(renderer.tools), "find /x -name f4") {
		t.Fatalf("completion dropped the detail it inherited: %q", formatToolActivity(renderer.tools))
	}
}

// Elapsed time has to come from a ticker rather than from the event stream,
// because a tool that blocks prints nothing and emits nothing. It also has to
// wait: a call that finishes fast never looked like it was waiting, and a
// duration on every line would be noise.
func TestRunningToolEarnsADurationOnlyAfterTheThreshold(t *testing.T) {
	renderer := &runRenderer{messageID: 1}
	renderer.tool("run-1", "🔄 >_ Run command · find /x -name f0")

	oldThreshold := toolElapsedThreshold
	toolElapsedThreshold = time.Hour
	if renderer.tickToolElapsed() {
		t.Fatal("a call that had barely started showed a duration it had not earned")
	}
	if strings.Contains(formatToolActivity(renderer.tools), "\U0001f552") {
		t.Fatalf("duration before the threshold: %q", formatToolActivity(renderer.tools))
	}

	toolElapsedThreshold = 0
	if !renderer.tickToolElapsed() {
		t.Fatal("crossing the threshold produced no duration")
	}
	if !strings.Contains(formatToolActivity(renderer.tools), "\U0001f552") {
		t.Fatalf("running line carries no duration: %q", formatToolActivity(renderer.tools))
	}
	// The displayed number is bucketed, so two looks inside one bucket render
	// the same line and there is nothing for a later edit to pick up.
	renderer.tickToolElapsed()
	before := renderer.streamTextLocked()
	renderer.tickToolElapsed()
	if after := renderer.streamTextLocked(); after != before {
		t.Fatalf("an unchanged bucket redrew the line: %q -> %q", before, after)
	}

	renderer.tool("run-1", "✅ >_ Run command completed")
	elapsed := renderer.tools[0].elapsed
	if elapsed == "" {
		t.Fatal("a call that showed a duration did not keep one on completion")
	}
	toolElapsedThreshold = oldThreshold
	t.Cleanup(func() { toolElapsedThreshold = oldThreshold })
}

// The ticker has to reach the message, not just the struct: without a signal
// the duration would sit in memory while the chat showed nothing.
func TestRunningToolTickerReachesThePreview(t *testing.T) {
	oldThreshold, oldTick := toolElapsedThreshold, toolElapsedTick
	toolElapsedThreshold = 0
	toolElapsedTick = 5 * time.Millisecond
	t.Cleanup(func() {
		toolElapsedThreshold = oldThreshold
		toolElapsedTick = oldTick
	})

	var mu sync.Mutex
	var seen []string
	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		// The live preview goes out as a rich edit, so the text is not always in
		// the "text" field of the payload.
		text, _ := payload["text"].(string)
		if rich, ok := payload["rich_message"].(map[string]any); ok {
			text, _ = rich["markdown"].(string)
		}
		if text != "" {
			mu.Lock()
			seen = append(seen, text)
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":31}}`))
	})

	renderer := newRunRenderer("token", 1)
	defer renderer.close()
	renderer.tool("run-1", "🔄 >_ Run command · find /x -name f0")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		found := false
		for _, text := range seen {
			if strings.Contains(text, "\U0001f552") {
				found = true
			}
		}
		mu.Unlock()
		if found {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("the ticker never reached the preview: %#v", seen)
}
