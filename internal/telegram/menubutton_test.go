package telegram

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A quick tunnel's hostname is minted per boot, so the button Telegram is
// holding after a restart points at a URL that is already dead. It has to be
// taken back to a plain label before the new handshake, or every tap in the
// startup window answers 1033 and reads as Ceraa being down.
func TestClearChatMenuButtonDropsTheWebApp(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	defer server.Close()
	oldBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = oldBase }()

	if err := clearChatMenuButton("t", "🌐 Web (starting)"); err != nil {
		t.Fatalf("clear: %s", err)
	}
	var sent struct {
		MenuButton struct {
			Type   string `json:"type"`
			Text   string `json:"text"`
			WebApp any    `json:"web_app"`
		} `json:"menu_button"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("body %s: %s", body, err)
	}
	if sent.MenuButton.Type != "default" {
		t.Errorf("menu_button type = %q, want default", sent.MenuButton.Type)
	}
	if sent.MenuButton.WebApp != nil {
		t.Errorf("cleared button still carries a web_app: %v", sent.MenuButton.WebApp)
	}
	if !strings.Contains(sent.MenuButton.Text, "starting") {
		t.Errorf("label %q does not say the surface is starting", sent.MenuButton.Text)
	}
}
