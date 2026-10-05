package telegram

import (
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
