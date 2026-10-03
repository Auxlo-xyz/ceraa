package telegram

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// stubAPI points the package at a test server for the rest of the test.
func stubAPI(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	oldBase := telegramAPIBase
	oldTrace := telegramTrace.Load()
	telegramAPIBase = server.URL + "/bot"
	telegramTrace.Store(false)
	t.Cleanup(func() {
		telegramAPIBase = oldBase
		telegramTrace.Store(oldTrace)
	})
	return server
}

func requestBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading request body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unreadable request body: %v", err)
	}
	return payload
}

// Telegram reports "message is not modified" as HTTP 400 with a JSON body.
// Returning on the status line first made the tolerant branch below unreachable,
// so every identical re-render came back as an error, left sentStream unset, and
// re-sent byte-identical text on the next tick. That loop is what took the
// recorded run to "retry after 227".
func TestANoOpEditIsASuccess(t *testing.T) {
	var calls atomic.Int64
	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`))
	})

	if err := editTelegramMessage("token", 1, 42, "same text"); err != nil {
		t.Fatalf("an edit refused as a no-op reported an error: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

// The tolerance above must not swallow a genuine rejection.
func TestAnEditStillSurfacesRealRejections(t *testing.T) {
	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`))
	})

	err := editTelegramMessage("token", 1, 42, strings.Repeat("x", 5000))
	if err == nil {
		t.Fatal("a rejected edit reported success")
	}
	if !strings.Contains(err.Error(), "message is too long") {
		t.Errorf("error = %v, want the API's own description", err)
	}
}

// The live API rejects editMessageText carrying rich_message while accepting the
// rich send beside it, so attempting it on every streaming tick bought a 400 on
// each one: 1999 in the recorded log, before falling through to the plain edit.
// One probe per run keeps the branch honest without repeating the round trip.
func TestARichEditIsPlacedOncePerRun(t *testing.T) {
	var richEdits, plainEdits atomic.Int64
	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		payload := requestBody(t, r)
		if _, rich := payload["rich_message"]; rich {
			richEdits.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"rich edit is unavailable"}`))
			return
		}
		plainEdits.Add(1)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":9}}`))
	})

	renderer := &runRenderer{token: "token", chatID: 1, messageID: 42}
	for i := 0; i < 5; i++ {
		renderer.mu.Lock()
		renderer.streamContent = "delta " + strconv.Itoa(i)
		renderer.mu.Unlock()
		renderer.renderCurrentState()
	}

	if got := richEdits.Load(); got != 1 {
		t.Errorf("rich edits placed = %d, want 1 (every later tick should take the plain path)", got)
	}
	if plainEdits.Load() == 0 {
		t.Error("the plain fallback was never used, so the preview never reached Telegram")
	}
}

// A rate limit is armed by the rejection itself. The recorded penalties were 78,
// 176, 178 and 227 seconds, and re-sending every 300ms through them is what kept
// the bucket empty, so the window is capped at a probing interval rather than

// While the window is open the preview must not spend calls it will lose, and it
// must leave sentStream untouched so the first render afterwards still pushes

// The published thought is a finished artifact, not a progress line: cutting its
// start would rewrite where the reasoning began.
func TestAPublishedThoughtKeepsItsOpening(t *testing.T) {
	long := strings.Repeat("alpha bravo charlie delta ", 300)
	got := formatThought(long, false)

	if !strings.Contains(got, "alpha bravo charlie delta") {
		t.Error("the opening of the thought was dropped")
	}
	if !strings.HasSuffix(got, "...</blockquote>") {
		t.Errorf("the dropped tail is unmarked: ...%q", got[len(got)-40:])
	}
	if len([]rune(thoughtBody(got))) > streamPreviewLimit {
		t.Errorf("published thought is %d runes, want at most %d", len([]rune(thoughtBody(got))), streamPreviewLimit)
	}
}

// A live preview keeps the newest text, but a truncated one must say so instead
// of opening as if the answer had started there.
func TestAStreamPreviewMarksTheDroppedHead(t *testing.T) {
	got := truncateStream(strings.Repeat("x", 5000))
	if !strings.HasPrefix(got, "...") {
		t.Error("a truncated preview does not report that text is missing")
	}
	if len([]rune(got)) != streamPreviewLimit {
		t.Errorf("length = %d runes, want %d", len([]rune(got)), streamPreviewLimit)
	}
	if unchanged := truncateStream("short"); unchanged != "short" {
		t.Errorf("short text was altered: %q", unchanged)
	}
}

// thoughtBody strips the blockquote wrapper so a limit assertion reads as a
// limit on the text the user actually sees.
func thoughtBody(value string) string {
	value = strings.TrimPrefix(value, "<blockquote expandable><b>Thought</b>\n\n")
	return strings.TrimSuffix(value, "</blockquote>")
}
