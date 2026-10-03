package telegram

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type probeCall struct {
	method    string
	messageID int
	parseMode string
	kind      string // rich | html | plain | draft
	text      string
}

type probeLog struct {
	mu    sync.Mutex
	calls []probeCall
}

func (p *probeLog) record(c probeCall) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, c)
}

func (p *probeLog) dump(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, c := range p.calls {
		body := c.text
		if len(body) > 54 {
			body = body[:54] + ".."
		}
		body = strings.ReplaceAll(body, "\n", "\\n")
		t.Logf("%3d  %-22s msg=%-4d %-5s %-5s %q", i, c.method, c.messageID, c.kind, c.parseMode, body)
	}
}

func (p *probeLog) count(pred func(probeCall) bool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if pred(c) {
			n++
		}
	}
	return n
}

func (p *probeLog) texts(pred func(probeCall) bool) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.calls {
		if pred(c) {
			out = append(out, c.text)
		}
	}
	return out
}

// thoughtProbe drives a streaming thought through the real renderer against a
// mock that behaves like Telegram: sends mint a fresh id, a rich edit is refused
// exactly as production refuses it, and a repeat of identical text comes back as
// "message is not modified". That last one is the response that decides whether
// the thought keeps its blockquote or falls back to plain text.
func thoughtProbe(t *testing.T, deltas []string, gap time.Duration, withContent bool) *probeLog {
	t.Helper()
	log := &probeLog{}
	var notModified atomic.Int64
	nextID := 100
	store := map[int]string{}
	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		call := probeCall{method: method}
		if id, ok := payload["message_id"].(float64); ok {
			call.messageID = int(id)
		}
		if pm, ok := payload["parse_mode"].(string); ok {
			call.parseMode = pm
		}
		if text, ok := payload["text"].(string); ok {
			call.text = text
		}
		if rm, ok := payload["rich_message"].(map[string]any); ok {
			call.kind = "rich"
			if md, ok := rm["markdown"].(string); ok {
				call.text = md
			}
		} else if method == "sendMessageDraft" {
			call.kind = "draft"
		} else if call.parseMode == "HTML" {
			call.kind = "html"
		} else {
			call.kind = "plain"
		}

		unchanged := func() bool {
			if prev, ok := store[call.messageID]; ok && prev == call.text {
				return true
			}
			store[call.messageID] = call.text
			return false
		}

		switch method {
		case "editMessageText":
			if call.kind == "rich" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"rich edit is unavailable"}`))
				return
			}
			if unchanged() {
				notModified.Add(1)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message"}`))
				return
			}
			log.record(call)
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":` + strconv.Itoa(call.messageID) + `}}`))
		case "sendMessageDraft":
			log.record(call)
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		case "deleteMessage", "editMessageReplyMarkup":
			log.record(call)
			delete(store, call.messageID)
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			nextID++
			call.messageID = nextID
			log.record(call)
			store[nextID] = call.text
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":` + strconv.Itoa(nextID) + `}}`))
		}
	})

	renderer := newRunRenderer("token", 1)
	for _, delta := range deltas {
		renderer.reasoning(delta)
		if withContent {
			renderer.content("word ")
		}
		time.Sleep(gap)
	}
	time.Sleep(600 * time.Millisecond)
	renderer.finish("done")
	t.Logf("not-modified rejections: %d", notModified.Load())
	return log
}

func thoughtDeltas() []string {
	return []string{
		"Let me look at the file first. ",
		"The function reads the config ",
		"and then it writes the cache. ",
		"Next I should check the tests ",
		"to see whether the seam is covered. ",
	}
}

// A streaming thought used to be rewritten into one message on every tick, and
// every rewrite makes the client redraw it. That redraw is the flicker, so the
// regression this guards is simple: nothing may reach editMessageText with the
// thought in it. The thought rides a draft while the run is live and is posted
// once, whole, when the run ends.
func TestAStreamingThoughtIsNeverRewrittenByEdit(t *testing.T) {
	if testing.Short() {
		t.Skip("probe")
	}
	for _, withContent := range []bool{false, true} {
		t.Logf("=== withContent=%v ===", withContent)
		log := thoughtProbe(t, thoughtDeltas(), 320*time.Millisecond, withContent)
		log.dump(t)

		const marker = "<blockquote expandable>"
		isThought := func(c probeCall) bool { return strings.Contains(c.text, marker) }

		if n := log.count(func(c probeCall) bool {
			return c.method == "editMessageText" && isThought(c)
		}); n != 0 {
			t.Errorf("withContent=%v: thought rewritten by editMessageText %d time(s)", withContent, n)
		}
		drafts := log.texts(func(c probeCall) bool { return c.method == "sendMessageDraft" && isThought(c) })
		if len(drafts) == 0 {
			t.Errorf("withContent=%v: the thought never reached a draft", withContent)
		} else {
			t.Logf("thought drafts: %d, first %d bytes, last %d bytes", len(drafts), len(drafts[0]), len(drafts[len(drafts)-1]))
		}
		posted := log.texts(func(c probeCall) bool {
			return c.method == "sendMessage" && c.parseMode == "HTML" && isThought(c)
		})
		if len(posted) != 1 {
			t.Errorf("withContent=%v: published thoughts = %d, want exactly 1", withContent, len(posted))
		} else if strings.Contains(posted[0], "▌") {
			t.Errorf("withContent=%v: published thought kept the streaming cursor", withContent)
		}
	}
}

// A server with no drafts must land back on the message path rather than lose
// the thought: the first refusal latches the transport off for the run, so the
// run pays for the probe exactly once and then behaves as it did before.
func TestAThoughtFallsBackToAMessageWhenDraftsAreRefused(t *testing.T) {
	var mu sync.Mutex
	var draftCalls int
	var thoughtWrites []string
	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessageDraft"):
			mu.Lock()
			draftCalls++
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat_id is not a private chat"}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage") && payload["parse_mode"] == "HTML":
			text, _ := payload["text"].(string)
			mu.Lock()
			thoughtWrites = append(thoughtWrites, text)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77}}`))
		case strings.HasSuffix(r.URL.Path, "/editMessageText") && payload["parse_mode"] == "HTML":
			text, _ := payload["text"].(string)
			mu.Lock()
			thoughtWrites = append(thoughtWrites, text)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77}}`))
		case strings.HasSuffix(r.URL.Path, "/sendRichMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":78}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		}
	})

	renderer := newRunRenderer("token", 1)
	renderer.reasoning("first step")
	renderer.reasoning(" second step")
	renderer.finish("done")

	mu.Lock()
	defer mu.Unlock()
	if draftCalls != 1 {
		t.Errorf("draft attempts = %d, want 1 (one refusal, then latched)", draftCalls)
	}
	if len(thoughtWrites) == 0 {
		t.Fatal("the thought was dropped instead of becoming a message")
	}
	if !strings.Contains(thoughtWrites[0], "first step") {
		t.Errorf("first thought write = %q", thoughtWrites[0])
	}
	final := thoughtWrites[len(thoughtWrites)-1]
	if !strings.Contains(final, "first step second step") {
		t.Errorf("published thought = %q, want both deltas", final)
	}
	if strings.Contains(final, "▌") {
		t.Errorf("published thought kept the streaming cursor: %q", final)
	}
}
