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
					if strings.HasPrefix(markdown, "## Heading") {
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
	renderer.throttleOverride = 5 * time.Millisecond
	renderer.content("## Heading")
	time.Sleep(50 * time.Millisecond)
	renderer.content("\n\n1. Step")
	time.Sleep(50 * time.Millisecond)
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
	if !strings.Contains(stream, "## Heading") {
		t.Fatalf("stream message = %q", stream)
	}
	// The preview carries no cursor. Telegram draws its own typing indicator on the
	// bot's message, so the block cursor was a second one, and it survived as a
	// stray glyph whenever a run ended without publishing.
	if strings.Contains(stream, "\u258c") {
		t.Fatalf("the preview still carries a cursor: %q", stream)
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
				// Gate on the preview edit itself. It used to gate on the cursor the
				// preview carried, and with the cursor gone no edit ever matched and
				// the test hung waiting for a signal that could not arrive.
				if strings.Contains(markdown, "growing") {
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
	renderer.throttleOverride = 5 * time.Millisecond
	renderer.content("Partial preview")
	time.Sleep(50 * time.Millisecond)
	renderer.content(" growing")
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
	renderer.content("## Heading")
	time.Sleep(150 * time.Millisecond)
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

// The thought is a message of its own, it is never replaced by a second
// assistant message, and the answer message is created only once there is
// something to put in it.
//
// Every id is recorded as it is created rather than assumed, because creation
// order is itself part of what is being pinned: the thought goes out first, and
// the assistant message appears once the run has content or an answer.
func TestThoughtStreamsInSeparateMessage(t *testing.T) {
	var mu sync.Mutex
	edits := make(map[int][]string)
	richSends := 0
	plainSends := 0
	nextID := 14
	assistantID := 0
	created := make(map[int]string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		textOf := func() string {
			text, _ := body["text"].(string)
			if rich, ok := body["rich_message"].(map[string]any); ok {
				text, _ = rich["markdown"].(string)
			}
			return text
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendRichMessage"),
			strings.HasSuffix(r.URL.Path, "/sendMessage") && body["parse_mode"] == "HTML":
			mu.Lock()
			if strings.HasSuffix(r.URL.Path, "/sendRichMessage") {
				richSends++
			} else {
				plainSends++
			}
			nextID++
			id := nextID
			created[id] = textOf()
			// The assistant message is identified by carrying the answer, not by
			// being created first: it is created when the run first has something
			// to say, which is not the first thing the run does.
			if !strings.HasPrefix(textOf(), "<blockquote>") {
				assistantID = id
			}
			mu.Unlock()
			_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, id)))
			return
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			id := int(body["message_id"].(float64))
			mu.Lock()
			edits[id] = append(edits[id], textOf())
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
	renderer.content("the answer")
	renderer.finish("done")
	renderer.close()

	mu.Lock()
	createdCopy := make(map[int]string, len(created))
	for id, text := range created {
		createdCopy[id] = text
	}
	assistantMessageID := assistantID
	answerEdits := append([]string(nil), edits[assistantID]...)
	richCount := richSends
	plainCount := plainSends
	mu.Unlock()

	if plainCount != 1 {
		t.Fatalf("thought messages = %d, want exactly one", plainCount)
	}
	// The block opens as a plain blockquote, which is the shape that can be
	// redrawn without anything to snap shut.
	thinkingID := 0
	for id, text := range createdCopy {
		if strings.HasPrefix(text, "<blockquote>") {
			thinkingID = id
		}
	}
	if thinkingID == 0 {
		t.Fatalf("the thought was never posted as its own message: %v", createdCopy)
	}
	if strings.Contains(createdCopy[thinkingID], "expandable") {
		t.Fatalf("streaming thought = %q, want a plain blockquote", createdCopy[thinkingID])
	}
	if assistantMessageID == 0 {
		t.Fatalf("no assistant message was created: %v", createdCopy)
	}
	if strings.HasPrefix(createdCopy[assistantMessageID], "<blockquote>") {
		t.Fatal("the thought and the answer share one message")
	}
	// One redraw for the whole run: the published thought, written once. The
	// intermediate state was paced out by the thought's own floor rather than
	// redrawn on every signal.
	published := edits[thinkingID]
	if len(published) != 1 {
		t.Fatalf("thought redraws = %#v, want only the published one", published)
	}
	if !strings.Contains(published[0], "<blockquote expandable>") || !strings.Contains(published[0], thoughtHeader) || !strings.Contains(published[0], "first step") {
		t.Fatalf("published thought = %#v", published[0])
	}
	if !strings.Contains(published[0], "first step\nsecond step") || strings.Contains(published[0], "\u258c") {
		t.Fatalf("published thought = %#v", published[0])
	}
	if len(answerEdits) == 0 || answerEdits[len(answerEdits)-1] != "done" {
		t.Fatalf("assistant edits = %#v", answerEdits)
	}
	// A cursor on its own is not a message. It was, and it sat at the top of every
	// run as an empty bubble between the prompt and the thought.
	if richCount != 1 {
		t.Fatalf("rich sends = %d, want exactly one assistant message", richCount)
	}
}

func TestToolActivityGetsItsOwnMessage(t *testing.T) {
	var mu sync.Mutex
	byID := make(map[int]string)
	sends := 0
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
			id := 14 + sends
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

	// Messages are told apart by what they carry, not by the order they were
	// created in. Creation order is not the contract: the trail appears whenever
	// the first tool reports, which can be before or after the answer message
	// exists.
	mu.Lock()
	trail := ""
	answer := ""
	trailCount := 0
	ids := make([]int, 0, len(byID))
	for id, text := range byID {
		ids = append(ids, id)
		if strings.Contains(text, "\U0001F4DA read") {
			trail = text
			trailCount++
			continue
		}
		answer = text
	}
	mu.Unlock()
	if trail == "" || trailCount != 1 {
		t.Fatalf("the tool trail is not exactly one message of its own: ids=%v answer=%q", ids, answer)
	}
	if !strings.Contains(trail, "\U0001F4DA read /workspace/ceraa") {
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
	if !strings.Contains(activity, "\U0001F4DA read file-1") {
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
	want := "\u26a1 bash find /workspace -name config.json"
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
	if strings.Contains(formatToolActivity(renderer.tools), "\U0001F550") {
		t.Fatalf("duration before the threshold: %q", formatToolActivity(renderer.tools))
	}

	toolElapsedThreshold = 0
	if !renderer.tickToolElapsed() {
		t.Fatal("crossing the threshold produced no duration")
	}
	if !strings.Contains(formatToolActivity(renderer.tools), "\U0001F550") {
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
			if strings.Contains(text, "\U0001F550") {
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

// A run that never reaches finish leaves a preview claiming to still be typing.
// It showed in a screenshot as a message with a live cursor minutes after the run
// was over, with nothing coming to replace it. Erroring and being stopped are the
// two ways a run ends without an answer, and both must retire the preview.
func TestAnAbandonedPreviewIsNeverLeftTyping(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave func(*runRenderer)
	}{
		{"failed", func(r *runRenderer) { r.fail() }},
		{"stopped", func(r *runRenderer) { r.stop() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			edits := make([]string, 0)
			deleted := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				switch {
				case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
					mu.Lock()
					deleted++
					mu.Unlock()
				case strings.HasSuffix(r.URL.Path, "/sendRichMessage"),
					strings.HasSuffix(r.URL.Path, "/sendMessage"):
					_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":21}}`))
					return
				case strings.HasSuffix(r.URL.Path, "/editMessageText"):
					text, _ := body["text"].(string)
					if rich, ok := body["rich_message"].(map[string]any); ok {
						text, _ = rich["markdown"].(string)
					}
					mu.Lock()
					edits = append(edits, text)
					mu.Unlock()
				}
				_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
			}))
			defer server.Close()
			oldBase := telegramAPIBase
			telegramAPIBase = server.URL + "/bot"
			defer func() { telegramAPIBase = oldBase }()

			renderer := newRunRenderer("token", 42)
			renderer.content("half an answer")
			tc.leave(renderer)
			renderer.close()

			// stop() retires the preview on its own goroutine once the render
			// goroutine has drained, so the last edit lands shortly after close()
			// returns rather than before it.
			deadline := time.Now().Add(2 * time.Second)
			var last string
			var gone int
			for {
				mu.Lock()
				if len(edits) > 0 {
					last = edits[len(edits)-1]
				}
				gone = deleted
				mu.Unlock()
				if last != "" || time.Now().After(deadline) {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if strings.Contains(last, "\u258c") {
				t.Fatalf("the abandoned preview still shows a live cursor: %q", last)
			}
			if !strings.Contains(last, "half an answer") {
				t.Fatalf("the text produced before the run ended was lost: %q", last)
			}
			if gone != 0 {
				t.Fatalf("a preview with content was deleted instead of finalised")
			}
		})
	}
}

// One message per thinking block and one per tool call.
//
// A single message for the whole run made every block of reasoning one bubble
// redrawn for the entire run, so the last block overwrote the first, and every
// tool call a line in a list that grew into a wall. The reference maps each entry
// to its own message id for the same reason.
func TestEveryThoughtBlockAndToolCallGetsItsOwnMessage(t *testing.T) {
	var mu sync.Mutex
	created := make([]string, 0)
	final := make(map[int]string)
	nextID := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		text, _ := body["text"].(string)
		if rich, ok := body["rich_message"].(map[string]any); ok {
			text, _ = rich["markdown"].(string)
		}
		mu.Lock()
		switch r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:] {
		case "sendRichMessage", "sendMessage":
			nextID++
			created = append(created, text)
			mu.Unlock()
			_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, 100+nextID)))
			return
		case "editMessageText":
			if n, ok := body["message_id"].(float64); ok {
				final[int(n)] = text
			}
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()
	oldBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = oldBase }()

	r := newRunRenderer("token", 42)
	r.reasoning("first block of reasoning")
	r.tool("c1", "🔄 "+toolNameToken("bash")+">_ Run command · ls -la /workspace")
	r.tool("c1", "✅ "+toolNameToken("bash")+">_ Run command completed")
	r.content("an answer in progress")
	r.reasoning("second block of reasoning")
	r.tool("c2", "🔄 "+toolNameToken("bash")+">_ Run command · du -sh /workspace")
	r.tool("c2", "✅ "+toolNameToken("bash")+">_ Run command completed")
	r.finish("the final answer")
	r.close()

	mu.Lock()
	defer mu.Unlock()
	thoughts, tools := 0, 0
	toolLines := make(map[string]int)
	for _, text := range created {
		switch {
		case strings.Contains(text, thoughtHeader):
			thoughts++
		case strings.HasPrefix(text, "\u26a1 bash"):
			tools++
			key := strings.Join(strings.Fields(text)[1:], " ")
			toolLines[key]++
		}
	}
	if thoughts != 2 {
		t.Errorf("thought messages = %d, want one per block (2): %q", thoughts, created)
	}
	if tools != 2 {
		t.Errorf("tool messages = %d, want one per call (2): %q", tools, created)
	}
	// Two calls that happen to share a prefix must not be merged into one
	// message, which is what reusing a single trail message did.
	for line, count := range toolLines {
		if count != 1 {
			t.Errorf("tool line %q appears in %d messages, want its own", line, count)
		}
	}
	// Every thought block is published rather than left carrying a live cursor.
	published := 0
	for _, text := range final {
		if strings.Contains(text, "blockquote expandable") && strings.Contains(text, thoughtHeader) {
			published++
			if strings.Contains(text, "\u258c") {
				t.Errorf("a published thought kept its cursor: %q", text)
			}
		}
	}
	if published != 2 {
		t.Errorf("published thought blocks = %d, want 2", published)
	}
}

// A provider may put its reasoning on the content channel, and then the thought
// and the answer carry the same text and the reader sees the thinking twice.
//
// Atria does this when no reasoning level was requested, which the run log
// recorded as reasoning="" next to an answer message that opened with the same
// sentence as the thought block above it. The two channels cannot be told apart
// from the deltas, because they interleave and the split lands mid-word, so the
// coordinator reports the boundary and the renderer folds the text back into the
// thought.
func TestReasoningArrivingOnTheContentChannelIsShownOnce(t *testing.T) {
	var mu sync.Mutex
	nextID := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewDecoder(r.Body).Decode(&map[string]any{})
		mu.Lock()
		switch r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:] {
		case "sendRichMessage", "sendMessage":
			nextID++
			mu.Unlock()
			_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, 100+nextID)))
			return
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()
	oldBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = oldBase }()

	renderer := newRunRenderer("token", 42)
	renderer.reasoning("The first cold build exceeded the timeout. The module cache should")
	// The provider keeps reasoning, but on the content channel.
	renderer.content(" be partially populated now, so a retry should be much faster.")
	// The step ends in a tool call, so none of it was an answer.
	renderer.markReasoningOnly()
	renderer.close()

	renderer.mu.Lock()
	thought := renderer.thought
	content := renderer.streamContent
	renderer.mu.Unlock()

	if strings.TrimSpace(content) != "" {
		t.Fatalf("the answer still carries text the thought is also showing: %q", content)
	}
	if !strings.Contains(thought, "partially populated now") {
		t.Fatalf("the reasoning did not reach the thought: %q", thought)
	}
	if !strings.Contains(thought, "The first cold build exceeded") {
		t.Fatalf("the reasoning that arrived on the content channel replaced the block instead of extending it: %q", thought)
	}
}

// A real answer is not reasoning, even when it arrives right after a thought, and
// the two channels routinely say similar things. Filing an answer as reasoning
// would leave the answer message empty for the whole run.
func TestAnAnswerAfterAThoughtIsNotFiledAsReasoning(t *testing.T) {
	r := &runRenderer{}

	r.reasoning("I should check the caches first.")
	r.content("Here is the report you asked for.")
	r.content(" The totals are below.")

	r.mu.Lock()
	thought := r.thought
	content := r.streamContent
	r.mu.Unlock()

	if content != "Here is the report you asked for. The totals are below." {
		t.Fatalf("answer = %q", content)
	}
	if strings.Contains(thought, "report") {
		t.Fatalf("the answer leaked into the thought: %q", thought)
	}
}

// Nothing is overwritten within a run, and nothing crosses between blocks.
//
// One message per run meant every fragment the model wrote during a long run was
// edited into the same bubble, each edit replacing the last, so the run ended
// showing only its final fragment. The run log showed it as one message whose text
// went "Container: ..." and then grew into a longer inventory line, which is the
// model rewriting its own notes rather than a reader watching prose arrive.
func TestEveryBlockGetsItsOwnMessageAndNothingIsOverwritten(t *testing.T) {
	var mu sync.Mutex
	created := make([]string, 0)
	edits := make(map[int][]string)
	nextID := 0
	idOf := func(created int) int { return 100 + created }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		text, _ := body["text"].(string)
		if rich, ok := body["rich_message"].(map[string]any); ok {
			text, _ = rich["markdown"].(string)
		}
		mu.Lock()
		switch r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:] {
		case "sendRichMessage", "sendMessage":
			nextID++
			created = append(created, text)
			mu.Unlock()
			_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, idOf(nextID))))
			return
		case "editMessageText":
			if n, ok := body["message_id"].(float64); ok {
				edits[int(n)] = append(edits[int(n)], text)
			}
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()
	oldBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = oldBase }()

	r := newRunRenderer("token", 42)
	// A step whose reasoning arrives on the content channel, the shape the log
	// recorded for this provider.
	r.reasoning("The user wants a machine inventory.")
	r.content(" Container: Debian 13.")
	r.markReasoningOnly()
	r.tool("c1", "🔄 "+toolNameToken("bash")+">_ Run command · df -h")
	r.tool("c1", "✅ "+toolNameToken("bash")+">_ Run command completed")
	// Prose written between two tool calls is its own block, not an edit of an
	// earlier one.
	r.content("Disk: 66G free on /workspace.")
	r.reasoning("Let me check the listeners.")
	r.tool("c2", "🔄 "+toolNameToken("bash")+">_ Run command · ss -tulnp")
	r.tool("c2", "✅ "+toolNameToken("bash")+">_ Run command completed")
	r.finish("Inventory complete.")
	r.close()

	mu.Lock()
	defer mu.Unlock()
	// Every block is a message of its own, and the count is not asserted directly:
	// whether the final answer lands as an edit on the message already streaming it
	// or needs a send of its own depends on whether the render goroutine drained
	// first, which is a race this test was asserting as if it were a contract. What
	// matters is that nothing is overwritten and nothing is duplicated, so both are
	// asserted below instead.
	if len(created) < 5 {
		t.Fatalf("messages = %d, want a message per block: %q", len(created), created)
	}
	// Reasoning that arrived on the content channel belongs to the thought, so no
	// message may start with it.
	for _, text := range created {
		if strings.HasPrefix(text, "Container:") {
			t.Fatalf("reasoning was shown as an answer message: %q", text)
		}
	}
	// The prose written before the second tool call survives on a message of its
	// own. It used to be edited into the same bubble the final answer went to,
	// which is the overwrite this test exists to catch.
	// Whether a fragment reaches the chat as a creation or as an edit depends on
	// whether the render goroutine drained before the tool call closed the block,
	// so both are searched. Scanning only the edits made this fail about one run in
	// three, which is a test that cannot be trusted rather than a product fault.
	found := false
	for _, text := range created {
		if strings.Contains(text, "66G free") {
			found = true
		}
	}
	for _, texts := range edits {
		for _, text := range texts {
			if strings.Contains(text, "66G free") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("the prose written mid-run was overwritten: %v", edits)
	}
	// No message carries the final answer and the mid-run fragment together.
	for id, texts := range edits {
		joined := strings.Join(texts, "\n")
		if strings.Contains(joined, "66G free") && strings.Contains(joined, "Inventory complete.") {
			t.Errorf("message %d holds two answers: %q", id, joined)
		}
	}
	// The final answer appears exactly once across the whole chat, whether it was
	// sent or edited into place. Two copies of it is the duplicate that a run log
	// showed as a streamed answer sitting next to its own final.
	appearances := 0
	for _, text := range created {
		if strings.Contains(text, "Inventory complete.") {
			appearances++
		}
	}
	for _, texts := range edits {
		for _, text := range texts {
			if strings.Contains(text, "Inventory complete.") {
				appearances++
			}
		}
	}
	if appearances != 1 {
		t.Errorf("the final answer appears %d time(s) in the chat, want exactly 1", appearances)
	}
}

// The final answer must land on the message that was already streaming it.
//
// The duplicate came from the step-boundary callback firing on the final step as
// well as on the ones that ended in tool calls. On the final step the content
// channel was carrying the answer rather than reasoning, so the renderer folded
// the answer into a thought and released the message it was streaming in, leaving
// the final publish with nothing to edit and a duplicate to send. It only
// reproduced when the last step also carried tool calls.
func TestTheFinalAnswerLandsOnTheMessageThatStreamedIt(t *testing.T) {
	var mu sync.Mutex
	created := make(map[int]string)
	edits := make(map[int][]string)
	nextID := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		text, _ := body["text"].(string)
		if rich, ok := body["rich_message"].(map[string]any); ok {
			text, _ = rich["markdown"].(string)
		}
		mu.Lock()
		switch r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:] {
		case "sendRichMessage", "sendMessage":
			nextID++
			id := 100 + nextID
			created[id] = text
			mu.Unlock()
			_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, id)))
			return
		case "editMessageText":
			if n, ok := body["message_id"].(float64); ok {
				id := int(n)
				edits[id] = append(edits[id], text)
			}
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()
	oldBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = oldBase }()

	r := newRunRenderer("token", 42)
	r.throttleOverride = 5 * time.Millisecond
	// A step that ends in a tool call: reasoning on the content channel, folded.
	r.reasoning("Checking the file.")
	r.content(" It is clean.")
	r.markReasoningOnly()
	r.tool("c1", "🔄 "+toolNameToken("bash")+">_ Run command · wc -l apitrace.go")
	r.tool("c1", "✅ "+toolNameToken("bash")+">_ Run command completed")
	// The final step: the answer streams, and the boundary callback fires here too.
	r.throttleOverride = 5 * time.Millisecond
	r.content("It is clean and matches its last commit.")
	time.Sleep(50 * time.Millisecond)
	r.finish("It is clean and matches its last commit.")
	r.close()

	mu.Lock()
	defer mu.Unlock()
	// Exactly one message may hold any part of the final answer, and it must be
	// the one that was created for it.
	holders := 0
	for id, texts := range edits {
		joined := strings.Join(texts, "\n")
		if strings.Contains(joined, "matches its last commit") {
			holders++
			if !strings.Contains(created[id], "clean and matches") {
				t.Errorf("the answer was edited onto message %d, which was created carrying %q", id, created[id])
			}
		}
	}
	for _, text := range created {
		if strings.Contains(text, "matches its last commit") {
			holders++
		}
	}
	if holders != 1 {
		t.Errorf("the final answer appears in %d messages, want 1: created=%v edits=%v", holders, created, edits)
	}
}
