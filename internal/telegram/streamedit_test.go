package telegram

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// countingTelegram mimics the production API closely enough to catch the real
// faults: rich edits on an existing message are rejected, and a plain edit
// carrying byte-identical text is rejected as "message is not modified".
//
// Both rejections are observed facts from the live incident, not assumptions: 413
// rejected rich edits and 404 no-op rejections, ending in 429 retry after 227s.
type countingTelegram struct {
	mu        sync.Mutex
	plain     []string
	rich      []string
	nextID    int
	rejected  int
	attempts  []string
	seenPairs map[string]int

	failContains string
	failOnce     bool
}

// pair identifies one message's rendered text. Telegram rejects a PUT that
// repeats a message's current text, so repeating a pair is the fault. Keying on
// the message id matters: the preview replacement targets a new id by design.
func pair(id int, text string) string {
	return strconv.Itoa(id) + "\x00" + text
}

func (c *countingTelegram) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		write := func(id int) {
			b, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"message_id": id}})
			_, _ = w.Write(b)
		}
		fail := func(desc string) {
			b, _ := json.Marshal(map[string]any{"ok": false, "error_code": 400, "description": desc})
			_, _ = w.Write(b)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			id := 0
			if n, isNum := body["message_id"].(float64); isNum {
				id = int(n)
			}
			if rm, isRich := body["rich_message"].(map[string]any); isRich {
				md, _ := rm["markdown"].(string)
				c.mu.Lock()
				c.rich = append(c.rich, md)
				c.mu.Unlock()
				// Production rejects rich edits on an existing message.
				fail("rich edit is unavailable")
				return
			}
			txt, _ := body["text"].(string)
			c.mu.Lock()
			c.attempts = append(c.attempts, txt)
			if c.failContains != "" && c.failOnce && strings.Contains(txt, c.failContains) {
				c.failOnce = false
				c.mu.Unlock()
				b, _ := json.Marshal(map[string]any{"ok": false, "error_code": 500, "description": "boom"})
				_, _ = w.Write(b)
				return
			}
			key := pair(id, txt)
			if c.seenPairs == nil {
				c.seenPairs = map[string]int{}
			}
			if c.seenPairs[key] > 0 {
				c.rejected++
			} else {
				c.seenPairs[key]++
				c.plain = append(c.plain, txt)
			}
			c.mu.Unlock()
			write(9)
		case strings.HasSuffix(r.URL.Path, "/sendRichMessage"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			rm, _ := body["rich_message"].(map[string]any)
			md, _ := rm["markdown"].(string)
			c.mu.Lock()
			c.rich = append(c.rich, md)
			c.nextID++
			id := 700 + c.nextID
			// The message now shows this text, so a later edit repeating it on the
			// same id is a no-op PUT. Without this the replacement seed is untestable.
			if c.seenPairs == nil {
				c.seenPairs = map[string]int{}
			}
			c.seenPairs[pair(id, md)]++
			c.mu.Unlock()
			write(id)
		default:
			write(9)
		}
	}))
}

func (c *countingTelegram) newRenderer(t *testing.T) (*runRenderer, func()) {
	t.Helper()
	srv := c.server(t)
	old := telegramAPIBase
	telegramAPIBase = srv.URL + "/bot"
	r := newRunRenderer("token", 42)
	return r, func() {
		r.close()
		telegramAPIBase = old
		srv.Close()
	}
}

// The preview replacement in reasoning() creates a new message already carrying
// the preview, then signals an edit of that same text. The signal is the fault:
// it PUTs bytes the message is already showing.
//
// The replacement itself targets a new id by design, so the invariant is per
// message id rather than per text. An earlier version of this test compared texts
// globally and failed on the replacement send itself, which is correct behaviour.
func TestReasoningDoesNotResendThePreviewItJustCreated(t *testing.T) {
	c := &countingTelegram{}
	r, done := c.newRenderer(t)
	defer done()

	// Render one state, then change it. The replacement carries the newer state, so
	// without the seed the signal below re-PUTs text the new message already shows.
	r.throttleOverride = 5 * time.Millisecond
	r.content("first state")
	time.Sleep(50 * time.Millisecond)
	r.content("second state")
	r.reasoning("thinking hard")
	time.Sleep(100 * time.Millisecond)
	r.close()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rejected != 0 {
		t.Errorf("a message was PUT identical text %d time(s): %v", c.rejected, c.plain)
	}
	// The replacement must still happen, or the thought never gets its own message.
	if len(c.rich) < 2 {
		t.Errorf("expected the replacement send, got %d rich call(s)", len(c.rich))
	}
}

// The original fault: a full-buffer PUT on every signal, whatever the content.
// Telegram answered 404 of these with "message is not modified" and the run hit
// 429 retry after 227s.
func TestStreamingNeverPutsIdenticalTextTwice(t *testing.T) {
	c := &countingTelegram{}
	r, done := c.newRenderer(t)
	defer done()

	const answer = "a completed answer that stops growing"
	r.throttleOverride = 5 * time.Millisecond
	r.content(answer)
	time.Sleep(50 * time.Millisecond)
	// Real tool values are human-readable labels. An earlier version of this test
	// passed JSON, which parseToolActivity rejects, so tool() returned early and
	// never signalled: green while exercising nothing.
	for i := 0; i < 6; i++ {
		r.reasoning("same thought every time")
		time.Sleep(20 * time.Millisecond)
	}
	r.finish(answer)
	r.close()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rejected != 0 {
		t.Errorf("Telegram rejected %d no-op edit(s); text was re-sent unchanged", c.rejected)
	}
}

// The cursor is gone from the answer preview, so the text a message is created
// with and the text it is first edited with are now identical. Telegram answers
// that "message is not modified", and it is a wasted call on every single run, so
// the creation is recorded as delivered rather than redrawn.
func TestAnAnswerIsNotReplacedByAnIdenticalEdit(t *testing.T) {
	var mu sync.Mutex
	posts := make([]string, 0)
	edits := make(map[int][]string)
	nextID := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		switch r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:] {
		case "sendRichMessage":
			rich, _ := body["rich_message"].(map[string]any)
			markdown, _ := rich["markdown"].(string)
			nextID++
			posts = append(posts, markdown)
			mu.Unlock()
			_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, 100+nextID)))
			return
		case "editMessageText":
			if n, ok := body["message_id"].(float64); ok {
				id := int(n)
				text, _ := body["text"].(string)
				if rich, ok := body["rich_message"].(map[string]any); ok {
					text, _ = rich["markdown"].(string)
				}
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

	const answer = "the completed answer"
	r := newRunRenderer("token", 42)
	r.content(answer)
	time.Sleep(300 * time.Millisecond)
	r.finish(answer)
	r.close()

	mu.Lock()
	defer mu.Unlock()
	created := false
	for _, text := range posts {
		if text == answer {
			created = true
		}
	}
	if !created {
		t.Fatalf("the answer message was never created carrying its own text: %q", posts)
	}
	for id, texts := range edits {
		for i, text := range texts {
			if text == answer && i == 0 && created {
				t.Errorf("message %d was re-PUT the exact text it was created with", id)
			}
		}
	}
}

// A rejected edit must not be recorded as delivered, or the message stays stale
// for the rest of the run.
//
// A repeated report for the same call updates the entry in place, so it
// re-renders identical text and is skipped, and tool() clears streamContent:
// neither is a genuine retry. The text has to change for one to be exercised.
func TestRejectedEditIsRetriedRatherThanTreatedAsSent(t *testing.T) {
	c := &countingTelegram{failContains: "retryme", failOnce: true}
	r, done := c.newRenderer(t)
	defer done()

	// The first report creates the trail message, which is a send and cannot be
	// refused the way an edit can. The text has to change afterwards for the
	// edits this test is about to happen at all, and the signals have to clear the
	// outbound floor, which production sets to a second.
	r.throttleOverride = 5 * time.Millisecond
	r.tool("call-1", "🔄 📚 Read · /tmp/other")
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 2; i++ {
		r.tool("call-1", "✅ 📚 Read completed · /tmp/retryme")
		time.Sleep(50 * time.Millisecond)
	}
	r.close()

	c.mu.Lock()
	defer c.mu.Unlock()
	attempts := 0
	for _, e := range c.attempts {
		if strings.Contains(e, "retryme") {
			attempts++
		}
	}
	if attempts < 2 {
		t.Errorf("the rejected text was attempted %d time(s); a failed edit was recorded as delivered: %v",
			attempts, c.attempts)
	}
}

// Pacing has to bound the whole run, not just content. tool() previously had no
// limit of its own, so a burst of results signalled on every event.
func TestToolBurstIsThrottled(t *testing.T) {
	c := &countingTelegram{}
	r, done := c.newRenderer(t)
	defer done()

	for i := 0; i < 40; i++ {
		r.tool("call-"+string(rune('a'+i%4)), "🔄 📚 Read file · /tmp/f"+string(rune('0'+i%10)))
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	r.close()

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.plain) > 6 {
		t.Errorf("%d edits for a 200ms burst; pacing is not bounding the run", len(c.plain))
	}
}
