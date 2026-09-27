package telegram

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// capturedCall is one API call the handler made.
type capturedCall struct {
	Method string
	Body   map[string]any
}

// capture is a recording buffer over the package-wide stub server.
//
// The server and the API base are installed once by TestMain rather than per test.
// handleCallback leaves a goroutine running after it returns, and that goroutine
// reads telegramAPIBase, so a test that restored the variable on cleanup raced
// with it. Only the buffer is swapped here, under a mutex, which is what the tests
// actually need to isolate.
type capture struct {
	mu    sync.Mutex
	calls []capturedCall
}

var (
	stubServer   *httptest.Server
	activeCap    *capture
	capMu        sync.Mutex
	installedAPI bool
)

// installStub points the Telegram API at a local recorder. Safe to call repeatedly.
func installStub() {
	capMu.Lock()
	defer capMu.Unlock()
	if installedAPI {
		return
	}
	stubServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		capMu.Lock()
		holder := activeCap
		capMu.Unlock()
		if holder != nil {
			holder.mu.Lock()
			holder.calls = append(holder.calls, capturedCall{Method: r.URL.Path, Body: body})
			holder.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	telegramAPIBase = stubServer.URL + "/bot"
	telegramClient = stubServer.Client()
	installedAPI = true
}

// beginCapture starts recording for one test.
func beginCapture(t *testing.T) *capture {
	t.Helper()
	installStub()
	holder := &capture{}
	capMu.Lock()
	activeCap = holder
	capMu.Unlock()
	t.Cleanup(func() {
		capMu.Lock()
		activeCap = nil
		capMu.Unlock()
	})
	return holder
}

// text returns every string delivered, joined, so a test can assert on content
// without caring which API call carried it.
func (c *capture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var parts []string
	for _, call := range c.calls {
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
	cap := beginCapture(t)
	tap(t, "menu:status")

	delivered := cap.text()
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
	statusCap := beginCapture(t)
	tap(t, "menu:status")
	statusText := statusCap.text()

	contextCap := beginCapture(t)
	tap(t, "menu:context")
	contextText := contextCap.text()

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
	cap := beginCapture(t)
	tap(t, "menu:permission")
	delivered := cap.text()
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
	buttonCap := beginCapture(t)
	tap(t, "menu:status")
	fromButton := buttonCap.text()

	statusCap := beginCapture(t)
	handleStatus("test-token", 1)
	fromStatusCommand := statusCap.text()

	contextCap := beginCapture(t)
	handleContextCommand("test-token", 1)
	fromContextCommand := contextCap.text()

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
