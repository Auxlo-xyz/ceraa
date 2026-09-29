package telegram

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Back into a menu has to re-render it, not replay the keyboard that was drawn when
// it was first opened.
//
// This is the reported bug. A hub was opened while xAI was active, the user went
// into a provider screen, switched to MiniMax, and pressed Back. The hub came back
// still naming xAI as active, because closeCallbackMenu restored the stored markup
// verbatim. Nothing logged, and a restart cleared it, so it looked like the switch
// needed a restart rather than like a stale screen.
func TestMenuRootRecordsTheMenuName(t *testing.T) {
	// The stored root has to know which menu it is, or Back has nothing to re-render.
	root := menuRoot{name: "hub", text: "📋 Ceraa hub", markup: map[string]any{}}
	if root.name != "hub" {
		t.Fatalf("menuRoot has no name: %+v", root)
	}
}

// An unnamed root keeps the stored copy, because a one-off keyboard is not a menu
// and there is nothing to re-render.
func TestUnnamedMenuRootHasNothingToRerender(t *testing.T) {
	var root menuRoot
	if root.name != "" {
		t.Errorf("an unnamed root should report no menu, got %q", root.name)
	}
}

// A re-render has to store what it just drew, or the menu can never be closed.
//
// The re-render branch returned without updating menuRootByMessage, so the stored
// snapshot kept the text the message carried when the menu was first opened.
// currentText then never equalled root.text, every later Close took the re-render
// branch again instead of the delete branch, and the message was permanently
// un-closeable. The second tap was then refused by Telegram as "message is not
// modified", which editMessageText logs and swallows, so the screen just stopped
// responding.
//
// The two tests beside this one assert that menuRoot has a name field, which is
// true of the struct no matter what the function does with it, so neither can
// fail. This drives closeCallbackMenu and compares what it drew against what it
// stored, which is the property that was missing.
//
// "status" is used because menuReportText re-renders it from live state, so the
// drawn body genuinely differs from the stale stored one. A name whose body is
// replayed unchanged cannot tell a refreshed snapshot from a stale one.
func TestReRenderRefreshesTheStoredRootSoCloseCanDelete(t *testing.T) {
	var drawn string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.HasSuffix(r.URL.Path, "editMessageText") && body.Text != "" {
			drawn = body.Text
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer server.Close()
	oldBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = oldBase }()

	const chatID int64 = 4242
	const messageID = 7
	const stale = "a screen from before the menu was ever re-rendered"
	menuRootByMessage.Store(menuTriggerKey(chatID, messageID), menuRoot{
		name:   "status",
		text:   stale,
		markup: map[string]any{"inline_keyboard": [][]map[string]string{{{}}}},
	})

	// Something moved the message on from what was stored, so Close re-renders.
	closeCallbackMenu("token", chatID, messageID, "unrelated text")

	if drawn == "" {
		t.Fatal("the re-render made no editMessageText call")
	}
	if drawn == stale {
		t.Fatalf("the re-render replayed the stale body %q, so this test cannot see staleness", stale)
	}

	root, ok := menuRootForMessage(chatID, messageID)
	if !ok {
		t.Fatal("the re-render discarded the stored root entirely")
	}
	if root.text != drawn {
		t.Errorf("stored root text = %q, want the text the re-render just drew (%q); "+
			"a stale snapshot makes every later Close re-render instead of delete", root.text, drawn)
	}

	// Now Close on the screen as it actually stands. It has to delete.
	var methods []string
	server.Close()
	recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, strings.TrimPrefix(r.URL.Path, "/bot/"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer recorder.Close()
	telegramAPIBase = recorder.URL + "/bot"

	closeCallbackMenu("token", chatID, messageID, drawn)

	edited, deleted := false, false
	for _, method := range methods {
		switch {
		case strings.HasSuffix(method, "editMessageText"):
			edited = true
		case strings.HasSuffix(method, "deleteMessage"):
			deleted = true
		}
	}
	if edited {
		t.Errorf("the second close re-rendered instead of deleting: %v", methods)
	}
	if !deleted {
		t.Errorf("the second close did not delete the menu: %v", methods)
	}
}
