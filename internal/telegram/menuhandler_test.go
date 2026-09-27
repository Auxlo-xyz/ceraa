package telegram

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capturedEdit is what the handler actually put on the wire.
type capturedEdit struct {
	Method string
	Body   map[string]any
}

// telegramStub captures every API call the handler makes.
type telegramStub struct {
	server *httptest.Server
	edits  []capturedEdit
	// sends captures sendMessage calls, which carry the text for menus that are
	// posted rather than edited.
	sends []map[string]any
}

func newTelegramStub(t *testing.T) *telegramStub {
	t.Helper()
	stub := &telegramStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if strings.Contains(r.URL.Path, "editMessage") {
			stub.edits = append(stub.edits, capturedEdit{Method: r.URL.Path, Body: body})
		} else {
			stub.sends = append(stub.sends, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))

	previousBase := telegramAPIBase
	previousClient := telegramClient
	telegramAPIBase = stub.server.URL + "/bot"
	telegramClient = stub.server.Client()
	t.Cleanup(func() {
		telegramAPIBase = previousBase
		telegramClient = previousClient
	})
	t.Cleanup(stub.server.Close)
	return stub
}

// text returns every string the stub was asked to deliver, joined, so a test can
// assert on content without caring which API call carried it.
func (s *telegramStub) text() string {
	var parts []string
	for _, call := range s.sends {
		if text, ok := call["text"].(string); ok {
			parts = append(parts, text)
		}
	}
	for _, call := range s.edits {
		if text, ok := call.Body["text"].(string); ok {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// tap fires a callback query through the real handler.
func tap(t *testing.T, data string) {
	t.Helper()
	q := &callbackQuery{ID: "cb1", Data: data}
	q.From.ID = 1
	q.Message = &struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		MessageID int    `json:"message_id"`
		Text      string `json:"text"`
	}{MessageID: 5}
	q.Message.Chat.ID = 1
	handleCallback("test-token", q)
}

// The reported bug: tapping the hub's Status button sent the menu's own name as
// the whole message, so the screen showed the word "status" above a row of
// buttons while /status printed a full report under an identical keyboard.
//
// Driven through the real handler with the API host pointed at a stub, because
// testing the body resolver directly passed even when the handler ignored it.
func TestTappingStatusSendsAReportOnTheWire(t *testing.T) {
	stub := newTelegramStub(t)
	tap(t, "menu:status")

	delivered := stub.text()
	if strings.TrimSpace(delivered) == "📋 status" {
		t.Fatalf("tapping Status delivered only the menu name: %q", delivered)
	}
	if delivered == "" {
		t.Fatal("tapping Status delivered no text at all")
	}
	for _, want := range []string{"Provider:", "Model:", "Session:", "Cache read:"} {
		if !strings.Contains(delivered, want) {
			t.Errorf("the delivered status text is missing %q:\n%s", want, delivered)
		}
	}
}

// The same path for the context alias, since both used to differ.
func TestTappingContextSendsTheSameReport(t *testing.T) {
	statusStub := newTelegramStub(t)
	tap(t, "menu:status")
	statusText := statusStub.text()

	contextStub := newTelegramStub(t)
	tap(t, "menu:context")
	contextText := contextStub.text()

	if statusText == "" || contextText == "" {
		t.Fatal("one of the two delivered nothing")
	}
	if statusText != contextText {
		t.Errorf("status and context deliver different text:\n--- status ---\n%q\n--- context ---\n%q", statusText, contextText)
	}
}

// An action menu must stay a title, or the fix turns every menu into a wall of the
// same report.
func TestTappingAnActionMenuSendsItsTitleNotTheReport(t *testing.T) {
	stub := newTelegramStub(t)
	tap(t, "menu:permission")
	delivered := stub.text()
	if strings.Contains(delivered, "Cache read:") {
		t.Errorf("the permission menu was given the status report:\n%s", delivered)
	}
	if !strings.Contains(delivered, "permission") {
		t.Errorf("the permission menu delivered %q, want a title naming it", delivered)
	}
}

// The typed commands have to agree with each other and with the buttons. /status
// and /context were two reports behind one keyboard, and the button showed
// neither; a test that only taps buttons would not have caught the commands
// drifting apart again.
func TestTypedCommandsDeliverTheSameReportAsTheButton(t *testing.T) {
	buttonStub := newTelegramStub(t)
	tap(t, "menu:status")
	fromButton := buttonStub.text()

	statusStub := newTelegramStub(t)
	handleStatus("test-token", 1)
	fromStatusCommand := statusStub.text()

	contextStub := newTelegramStub(t)
	handleContextCommand("test-token", 1)
	fromContextCommand := contextStub.text()

	for name, delivered := range map[string]string{
		"/status":  fromStatusCommand,
		"/context": fromContextCommand,
	} {
		if strings.TrimSpace(delivered) == "" {
			t.Errorf("%s delivered nothing", name)
			continue
		}
		if strings.TrimSpace(delivered) == "📋 status" {
			t.Errorf("%s delivered only the menu name", name)
			continue
		}
		if delivered != fromButton {
			t.Errorf("%s disagrees with the Status button:\n--- button ---\n%q\n--- %s ---\n%q", name, fromButton, name, delivered)
		}
	}
}
