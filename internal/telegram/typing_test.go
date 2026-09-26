package telegram

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// withFakeTelegram points the API at a local server and records the chat actions
// it receives.
type fakeTelegram struct {
	mu      sync.Mutex
	actions []string
	chats   []int64
	calls   int
	status  int
}

func (f *fakeTelegram) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !containsPath(r.URL.Path, "sendChatAction") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var payload struct {
			ChatID int64  `json:"chat_id"`
			Action string `json:"action"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		f.mu.Lock()
		f.actions = append(f.actions, payload.Action)
		f.chats = append(f.chats, payload.ChatID)
		f.calls++
		f.mu.Unlock()
		status := f.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	old := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	t.Cleanup(func() {
		telegramAPIBase = old
		server.Close()
	})
	oldClient := typingClient
	typingClient = server.Client()
	t.Cleanup(func() { typingClient = oldClient })
	return server
}

func (f *fakeTelegram) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeTelegram) lastAction() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.actions) == 0 {
		return ""
	}
	return f.actions[len(f.actions)-1]
}

func containsPath(path, want string) bool {
	return len(path) >= len(want) && path[len(path)-len(want):] == want
}

// TestTypingIndicatorSendsImmediately pins that a run shows feedback at once,
// rather than after the first refresh tick.
func TestTypingIndicatorSendsImmediately(t *testing.T) {
	fake := &fakeTelegram{}
	fake.start(t)

	typing := newTypingIndicator("token", 4242)
	typing.Start()
	defer typing.Stop()

	waitFor(t, func() bool { return fake.count() >= 1 })
	fake.mu.Lock()
	action, chat := fake.actions[0], fake.chats[0]
	fake.mu.Unlock()
	if action != "typing" {
		t.Fatalf("action = %q, want typing", action)
	}
	if chat != 4242 {
		t.Fatalf("chat_id = %d, want 4242", chat)
	}
}

// TestTypingIndicatorRefreshes pins the loop itself: Telegram expires a chat
// action after about five seconds, so one call at the start of a long run leaves
// the user staring at nothing.
func TestTypingIndicatorRefreshes(t *testing.T) {
	fake := &fakeTelegram{}
	fake.start(t)
	typing := newTypingIndicator("token", 1)
	typing.interval = 20 * time.Millisecond
	typing.Start()
	defer typing.Stop()

	waitFor(t, func() bool { return fake.count() >= 4 })
}

// TestTypingIndicatorStopsCleanly pins that the indicator stops when the agent
// does, and that Stop does not block.
func TestTypingIndicatorStopsCleanly(t *testing.T) {
	fake := &fakeTelegram{}
	fake.start(t)
	typing := newTypingIndicator("token", 1)
	typing.interval = 20 * time.Millisecond
	typing.Start()
	waitFor(t, func() bool { return fake.count() >= 1 })

	done := make(chan struct{})
	go func() {
		typing.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked")
	}

	// No refresh may land after the run is reported complete.
	settled := fake.count()
	time.Sleep(120 * time.Millisecond)
	if fake.count() != settled {
		t.Fatal("an action was sent after Stop")
	}
}

// TestTypingIndicatorStartAndStopAreIdempotent pins that the lifecycle calls can
// be repeated safely, which is what makes it safe to defer Stop and also call it
// on an exit path.
func TestTypingIndicatorStartAndStopAreIdempotent(t *testing.T) {
	fake := &fakeTelegram{}
	fake.start(t)

	typing := newTypingIndicator("token", 1)
	typing.Stop() // before any Start
	typing.Start()
	typing.Start() // already running
	typing.Stop()
	typing.Stop()
	// A late Start must not resurrect an action for a finished run.
	typing.Start()
	time.Sleep(80 * time.Millisecond)
	settled := fake.count()
	time.Sleep(80 * time.Millisecond)
	if fake.count() != settled {
		t.Fatal("a stopped indicator was restarted")
	}
}

// TestTypingIndicatorSurvivesAFailingChat pins that a chat where the action is not
// permitted does not break the run or retry forever.
func TestTypingIndicatorSurvivesAFailingChat(t *testing.T) {
	fake := &fakeTelegram{status: http.StatusForbidden}
	fake.start(t)
	typing := newTypingIndicator("token", 1)
	typing.interval = 20 * time.Millisecond
	typing.Start()
	waitFor(t, func() bool { return typing.Failures() >= 2 })
	typing.Stop()
}

// TestTypingIndicatorIsOptional pins that an unconfigured bot does no work at all.
func TestTypingIndicatorIsOptional(t *testing.T) {
	fake := &fakeTelegram{}
	fake.start(t)

	newTypingIndicator("", 0).Start()
	newTypingIndicator("", 0).Stop()
	var nilIndicator *typingIndicator
	nilIndicator.Start()
	nilIndicator.Stop()
	if fake.count() != 0 {
		t.Fatal("nothing should be sent without a token and chat")
	}
}

// TestTypingIntervalIsUnderTheExpiry pins the reason the interval is what it is:
// a refresh sent after Telegram has already dropped the action would leave a gap.
func TestTypingIntervalIsUnderTheExpiry(t *testing.T) {
	const telegramActionLifetime = 5 * time.Second
	if typingInterval >= telegramActionLifetime {
		t.Fatalf("refresh interval %s must be under the %s action lifetime", typingInterval, telegramActionLifetime)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met in time")
}
