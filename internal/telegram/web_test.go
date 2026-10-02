package telegram

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ceraa/internal/miniapp"
)

// The command opens the surface instead of reporting that it is off. A refusal
// was a sentence about configuration when what was asked for is a link, and it
// handed the operator work the program can do itself.
func TestWebShareOpensTheSurfaceInsteadOfRefusing(t *testing.T) {
	isolateState(t)
	captured := beginCapture(t)
	opened := false
	start := func() (*miniapp.Server, error) {
		opened = true
		return nil, errors.New("session store unavailable: nothing to open")
	}
	handleWebShare("test-token", 1, start)

	if !opened {
		t.Fatal("the command never asked for the surface to be opened")
	}
	text := captured.text()
	if strings.Contains(text, "CERAA_MINIAPP=1") {
		t.Errorf("reply = %q, still tells the operator to set a flag and restart", text)
	}
	if !strings.Contains(text, "session store unavailable") {
		t.Errorf("reply = %q, want the reason it could not open", text)
	}
	if strings.Contains(text, "https://") {
		t.Errorf("reply = %q, want no address when nothing was opened", text)
	}
}

// The command has to be in the catalog as well as in the dispatcher, because
// the catalog is what puts it in Telegram's own command menu and in /help. A
// case in the switch with no catalog entry is a command nobody discovers.
func TestWebShareIsAdvertisedAsACommand(t *testing.T) {
	for _, command := range commandCatalog() {
		if command.Command != "web" {
			continue
		}
		if command.Description == "" {
			t.Error("/web is advertised with no description")
		}
		return
	}
	t.Error("/web is not in the command catalog, so /help will not mention it")
}

// A quick tunnel mints a new hostname every time it starts. Publishing the
// address only as text therefore leaves a URL that works until the next restart
// and then fails as Cloudflare 1033, which names the symptom and nothing that
// caused it. The menu button is the one surface Telegram lets the bot rewrite, so
// publishing has to move it.
//
// Asserted through the request body rather than through the reply, because the
// reply was already correct before this change and would pass either way.
func TestPublishingTheAddressMovesTheMenuButton(t *testing.T) {
	isolateState(t)
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		if strings.HasSuffix(r.URL.Path, "/setChatMenuButton") {
			bodies = append(bodies, string(raw))
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	previous := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = previous }()

	const address = "https://example.trycloudflare.com"
	publishMiniAppAddress("test-token", 1, address)

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("setChatMenuButton called %d time(s), want 1: the button still points wherever "+
			"BotFather was last told", len(bodies))
	}
	body := bodies[0]
	if !strings.Contains(body, address) {
		t.Errorf("the button was not given the live address: %s", body)
	}
	if !strings.Contains(body, `"type":"web_app"`) {
		t.Errorf("the button is not a web_app button, so it would not open a Mini App: %s", body)
	}
}

// The address still reaches the chat when the button cannot be moved. Reporting
// no address at all because a convenience failed would make this strictly worse
// than the state it replaces.
func TestTheAddressIsSentEvenWhenTheButtonCannotBeMoved(t *testing.T) {
	isolateState(t)
	var mu sync.Mutex
	var delivered []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		if strings.HasSuffix(r.URL.Path, "/setChatMenuButton") {
			_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: chat not found"}`))
			mu.Unlock()
			return
		}
		delivered = append(delivered, string(raw))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer server.Close()
	previous := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = previous }()

	const address = "https://example.trycloudflare.com"
	publishMiniAppAddress("test-token", 1, address)

	mu.Lock()
	defer mu.Unlock()
	for _, body := range delivered {
		if strings.Contains(body, address) {
			return
		}
	}
	t.Errorf("the address was not delivered to the chat after the button could not be set; "+
		"sent %d message(s)", len(delivered))
}

// The watchdog is the only thing standing between a dead tunnel client and a
// menu button that advertises a hostname resolving nowhere. It runs on its own
// goroutine against state the command path also writes, so the two agreeing on
// one recorded address is the property worth asserting.
func TestTheWatchdogAndTheCommandPathAgreeOnOnePublishedAddress(t *testing.T) {
	isolateState(t)
	noteMiniAppPublished("https://first.trycloudflare.com")
	if got := miniAppPublishedOrigin(); got != "https://first.trycloudflare.com" {
		t.Fatalf("published origin = %q, want the first address", got)
	}
	noteMiniAppPublished("https://second.trycloudflare.com")
	if got := miniAppPublishedOrigin(); got != "https://second.trycloudflare.com" {
		t.Fatalf("published origin = %q, want the second address", got)
	}
}

// Recording the address is what stops the watchdog re-sending the button on every
// tick. If publishing did not record, the watchdog would compare against empty
// and republish forever, which is a quieter version of the fault it exists to
// repair.
func TestPublishingRecordsTheAddressTheWatchdogComparesAgainst(t *testing.T) {
	isolateState(t)
	noteMiniAppPublished("")
	var mu sync.Mutex
	seen := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	previous := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = previous }()

	const address = "https://recorded.trycloudflare.com"
	publishMiniAppAddress("test-token", 1, address)

	mu.Lock()
	defer mu.Unlock()
	if seen != 2 {
		t.Fatalf("publishing made %d API calls, want 2: one to the chat and one for the button", seen)
	}
	if got := miniAppPublishedOrigin(); got != address {
		t.Errorf("published origin = %q, want %q; the watchdog would republish every tick", got, address)
	}
}
