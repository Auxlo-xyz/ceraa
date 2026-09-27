package telegram

import (
	"strings"
	"testing"
)

// captureTrace redirects trace lines into the active capture for the duration of
// the test, so a test can assert on exactly what the logger would have written.
func captureTrace(t *testing.T) *capture {
	t.Helper()
	cap := beginCapture(t)
	sink := func(line string) {
		cap.mu.Lock()
		cap.trace = append(cap.trace, line)
		cap.mu.Unlock()
	}
	traceObserver.Store(&sink)
	t.Cleanup(func() { traceObserver.Store(nil) })
	return cap
}

// traceLines returns the trace lines recorded while tracing was on.
func (c *capture) traceLines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.trace...)
}

// The reply_markup kind is the whole point of the trace. A call that carries no
// markup, or the wrong kind, is what a stripped keyboard looks like from the
// bot's side, and nothing else in the log would reveal it.
func TestMarkupKindClassification(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no markup", `{"chat_id":1,"text":"hi"}`, "none"},
		{"reply keyboard", `{"chat_id":1,"text":"hi","reply_markup":{"keyboard":[[{"text":"a"}]],"is_persistent":true}}`, "reply_keyboard"},
		{"inline keyboard", `{"chat_id":1,"text":"hi","reply_markup":{"inline_keyboard":[]}}`, "inline_keyboard"},
		{"remove keyboard", `{"chat_id":1,"text":"hi","reply_markup":{"remove_keyboard":true}}`, "remove_keyboard"},
		{"force reply", `{"chat_id":1,"text":"hi","reply_markup":{"force_reply":true}}`, "force_reply"},
		{"unparseable", `not json`, "unparseable"},
	}
	for _, tc := range cases {
		if got := markupKind([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: markupKind = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The method name is taken from the URL, and the bot token is in that URL. If the
// token ever reached the log it would be a credential leak, so the extraction is
// pinned to the last path segment.
func TestMethodFromURLNeverIncludesTheToken(t *testing.T) {
	const token = "123456:SECRET-TOKEN-VALUE"
	method := methodFromURL("https://api.telegram.org/bot" + token + "/sendMessage")
	if method != "sendMessage" {
		t.Errorf("methodFromURL = %q, want sendMessage", method)
	}
	if strings.Contains(method, token) || strings.Contains(method, "SECRET") {
		t.Errorf("the token leaked into the method name: %q", method)
	}
}

// ok:true only means Telegram accepted the request. A keyboard that a client
// silently ignored produces the same verdict as one that worked, which is exactly
// why the earlier failures were invisible, so the verdict has to be logged.
func TestAcceptedCallsAreDistinguishableFromRejectedOnes(t *testing.T) {
	cap := captureTrace(t)
	// Restore whatever the boot default was, rather than forcing it off: the
	// default is on, and pinning it here would leak state into the next test.
	was := TelegramTraceEnabled()
	SetTelegramTrace(true)
	t.Cleanup(func() { SetTelegramTrace(was) })

	send("test-token", 1, "hello")
	if len(cap.traceLines()) == 0 {
		t.Fatal("tracing was on but nothing was traced")
	}
	// The trace must say the API accepted it, and must name the markup kind.
	joined := strings.Join(cap.traceLines(), "\n")
	if !strings.Contains(joined, "markup=reply_keyboard") {
		t.Errorf("the trace did not report the markup kind: %s", joined)
	}
	if !strings.Contains(joined, "verdict=ok") {
		t.Errorf("the trace did not report the API verdict: %s", joined)
	}
}

// Tracing is on at boot, so a misbehaving chat leaves evidence even if nobody
// thinks to turn it on first. /apilog off turns it down.
func TestTracingIsOnAtBoot(t *testing.T) {
	if !TelegramTraceEnabled() {
		t.Error("API tracing is off at boot, so a silent failure would leave no evidence")
	}
}
