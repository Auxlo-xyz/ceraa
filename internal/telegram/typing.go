package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// The typing indicator is a status signal, not a message, and Telegram expires it
// after about five seconds. So it has to be refreshed for as long as the agent is
// working, and stopped the moment it is not. A single call at the start of a run
// therefore goes dark partway through every long task, which reads as the agent
// having stalled.

// typingInterval is deliberately under the roughly five second lifetime Telegram
// gives an action, so a refresh never lands after the indicator has already gone.
const typingInterval = 4 * time.Second

// typingIndicator keeps a chat action alive while a run is in flight.
//
// It is safe to start and stop more than once, and safe to stop from a different
// goroutine than the one that started it, which is the normal case: the run is
// driven elsewhere and only reports back when it ends.
type typingIndicator struct {
	token  string
	chatID int64

	// interval is a field rather than a constant so a test can observe the
	// refresh loop without waiting four seconds per tick, and so a global is not
	// written while a refresh loop may be reading it.
	interval time.Duration

	mu      sync.Mutex
	stop    chan struct{}
	done    chan struct{}
	stopped bool
	// failures counts refreshes that did not land, so a chat where the action is
	// not permitted does not retry forever silently.
	failures int
}

func newTypingIndicator(token string, chatID int64) *typingIndicator {
	return &typingIndicator{token: token, chatID: chatID, interval: typingInterval}
}

// Start begins refreshing. Calling it again while already running does nothing.
func (t *typingIndicator) Start() {
	if t == nil || t.token == "" || t.chatID == 0 {
		return
	}
	t.mu.Lock()
	if t.stopped {
		// A stopped indicator stays stopped, so a late Start cannot resurrect an
		// action for a run that has already finished.
		t.mu.Unlock()
		return
	}
	if t.stop != nil {
		t.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	t.stop = stop
	t.done = done
	t.mu.Unlock()

	t.send()
	go t.loop(stop, done)
}

func (t *typingIndicator) loop(stop, done chan struct{}) {
	defer close(done)
	interval := t.interval
	if interval <= 0 {
		interval = typingInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			t.send()
		}
	}
}

// Stop ends the indicator and waits for the refresh loop to finish, so no action
// is sent after the run is reported complete.
func (t *typingIndicator) Stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	stop, done := t.stop, t.done
	t.stop, t.done = nil, nil
	t.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// send performs one refresh. A failure is counted and otherwise ignored: the
// indicator is cosmetic, and failing a run over it would be absurd.
func (t *typingIndicator) send() {
	if err := postChatAction(t.token, t.chatID, "typing"); err != nil {
		t.mu.Lock()
		t.failures++
		t.mu.Unlock()
	}
}

// Failures reports how many refreshes did not land, for diagnostics.
func (t *typingIndicator) Failures() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failures
}

// postChatAction sends the action on its own, without the retry ladder that real
// messages use. An indicator is cosmetic, so spending fifteen seconds retrying one
// inside the refresh loop would delay the next refresh and accomplish nothing.
func postChatAction(token string, chatID int64, action string) error {
	if token == "" || chatID == 0 || action == "" {
		return nil
	}
	body, err := json.Marshal(map[string]any{"chat_id": chatID, "action": action})
	if err != nil {
		return err
	}
	response, err := typingClient.Post(api("sendChatAction", token), "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("sendChatAction: HTTP %d", response.StatusCode)
	}
	return nil
}

// typingClient has a short timeout because the refresh loop must not outlive the
// run it is reporting on.
var typingClient = &http.Client{Timeout: 10 * time.Second}
