package telegram

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ceraa/internal/telegram/menus"
)

// /purge has to be discoverable: it is the escape hatch for a stale cached
// button, so a person reaching for it must find it in the menu and /help.
func TestPurgeCommandListed(t *testing.T) {
	for _, spec := range menus.Commands() {
		if spec.Command == "purge" {
			return
		}
	}
	t.Error("purge missing from the command catalog")
}

// The whole point of the purge is a link that cannot be serving a dead
// hostname, so the inline button must carry a web_app URL rather than a plain
// url. Telegram resolves an inline web_app at tap time; a plain link is fetched
// and cached like the menu button, which is the failure being fixed.
func TestPurgeKeyboardCarriesAWebApp(t *testing.T) {
	markup := purgeKeyboard("https://fresh.trycloudflare.com")
	rows, ok := markup["inline_keyboard"].([][]map[string]any)
	if !ok || len(rows) == 0 || len(rows[0]) == 0 {
		t.Fatalf("markup shape wrong: %#v", markup)
	}
	button := rows[0][0]
	webApp, ok := button["web_app"].(map[string]any)
	if !ok {
		t.Fatalf("button has no web_app: %#v", button)
	}
	if webApp["url"] != "https://fresh.trycloudflare.com" {
		t.Errorf("web_app url = %v", webApp["url"])
	}
	if _, hasPlainURL := button["url"]; hasPlainURL {
		t.Error("button carries a plain url, which Telegram caches like the menu button")
	}
	if !strings.Contains(button["text"].(string), "Open") {
		t.Errorf("label = %v", button["text"])
	}
}

// An automatic republish must not send a chat message.
//
// It used to, and the owner was right to object: a restart asked for a restart,
// not for a second link to compare against the first. The button is the surface
// the address belongs on, so it is the only thing allowed to move.
func TestAutomaticRepublishSendsNoMessage(t *testing.T) {
	var messages, menuCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			messages++
		case strings.HasSuffix(r.URL.Path, "/setChatMenuButton"):
			menuCalls++
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	defer server.Close()
	previousBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	t.Cleanup(func() { telegramAPIBase = previousBase })

	republishMiniAppAddress("t", nil, "https://fresh.trycloudflare.com")

	if menuCalls != 1 {
		t.Errorf("menu button set %d time(s), want 1", menuCalls)
	}
	if messages != 0 {
		t.Errorf("automatic republish sent %d chat message(s), want none", messages)
	}
}
