package telegram

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ceraa/internal/qoderauth"
	"ceraa/internal/secret"
	"ceraa/internal/telegram/menus"
)

// isolateQoderSecrets points the credential store at a temp directory: the
// login flow mints a machine id into it, and a test must not plant one in the
// live install.
func isolateQoderSecrets(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
}

type qoderNotifications struct {
	mu   sync.Mutex
	seen []string
}

func (n *qoderNotifications) snapshot() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.seen...)
}

func captureQoderNotify(t *testing.T) *qoderNotifications {
	t.Helper()
	seen := &qoderNotifications{}
	original := qoderNotify
	qoderNotify = func(token string, chatID int64, text string, kb map[string]any) {
		seen.mu.Lock()
		seen.seen = append(seen.seen, text)
		seen.mu.Unlock()
	}
	t.Cleanup(func() { qoderNotify = original })
	return seen
}

// The button existing and being handled are separate facts.
func TestProviderQoderCallbackReachesTheEntryPoint(t *testing.T) {
	var mu sync.Mutex
	called := 0
	original := qoderEntryPoint
	qoderEntryPoint = func(token string, chatID int64) {
		mu.Lock()
		called++
		mu.Unlock()
	}
	t.Cleanup(func() { qoderEntryPoint = original })

	var query callbackQuery
	if err := json.Unmarshal([]byte(`{"id":"1","data":"provider:qoder","message":{"chat":{"id":5151},"message_id":5}}`), &query); err != nil {
		t.Fatalf("callback: %v", err)
	}
	handleCallback("test-token", &query)

	mu.Lock()
	defer mu.Unlock()
	if called != 1 {
		t.Errorf("provider:qoder reached the entry point %d time(s); the button is dead", called)
	}
}

// "Sign in again" must start a fresh login, not silently do nothing.
func TestQoderReloginStartsALogin(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	isolateQoderSecrets(t)
	seen := captureQoderNotify(t)

	originalPoll := qoderPollLogin
	qoderPollLogin = func(*qoderauth.Manager, *qoderauth.Flow) error { return nil }
	defer func() { qoderPollLogin = originalPoll }()

	var query callbackQuery
	if err := json.Unmarshal([]byte(`{"id":"1","data":"provider:qoder:relogin","message":{"chat":{"id":5151},"message_id":5}}`), &query); err != nil {
		t.Fatalf("callback: %v", err)
	}
	handleCallback("test-token", &query)

	deadline := time.Now().Add(5 * time.Second)
	for {
		// The poll runs in the background, so the login message arrives
		// after the callback returns, and so does the terminal report.
		// Waiting for both is what keeps restore() from racing that
		// goroutine: the terminal notify is the last thing it sends, and it
		// reads the provider state before sending it, so seeing it here
		// orders that read ahead of the restore. Without this the test
		// returns while the goroutine is still reading providers, and the
		// cleanup write races it.
		notified := seen.snapshot()
		joined := strings.Join(notified, "\n")
		if strings.Contains(joined, "qoder.com/device/selectAccounts") && len(notified) >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider:qoder:relogin sent no login instructions; notified:\n%v", seen.snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A finished login with an owner switches provider when that is still wanted.
func TestQoderLoginSuccessActivates(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureQoderNotify(t)

	// Mirror production: the user was on opencode when they tapped switch, so
	// the owned login still wants the switch when it lands.
	if err := providers.setActive("opencode"); err != nil {
		t.Fatal(err)
	}
	trackQoderFlow("flow-1", 5151, true, "opencode")
	deliverQoderTerminal("test-token", "flow-1", true, "")

	if got := providers.active(); got != "qoder" {
		t.Errorf("active = %q after a successful owned login; want qoder", got)
	}
	joined := strings.Join(seen.snapshot(), "\n")
	if !strings.Contains(joined, "Qoder provider activated") {
		t.Errorf("no activation confirmation; notified:\n%v", seen.snapshot())
	}
	if _, ok := qoderFlowOwnerOf("flow-1"); ok {
		t.Error("the flow was not cleared after delivery")
	}
}

// A failed login tells the chat plainly instead of going quiet.
func TestQoderLoginFailureNotifies(t *testing.T) {
	seen := captureQoderNotify(t)

	trackQoderFlow("flow-2", 5151, true, "opencode")
	deliverQoderTerminal("test-token", "flow-2", false, "the user declined")

	joined := strings.Join(seen.snapshot(), "\n")
	if !strings.Contains(joined, "did not complete") {
		t.Errorf("no failure report; notified:\n%v", seen.snapshot())
	}
}

// A login nobody owns is logged, not delivered nowhere.
func TestQoderLoginWithoutOwnerIsDropped(t *testing.T) {
	seen := captureQoderNotify(t)
	deliverQoderTerminal("test-token", "no-such-flow", true, "")
	if got := seen.snapshot(); len(got) != 0 {
		t.Errorf("an ownerless login notified the chat: %v", got)
	}
}

// The model list is scoped to Qoder: directory keys route the encrypted
// endpoint, and a foreign name would write a profile the endpoint rejects.
func TestQoderModelMenuIsScopedToQoder(t *testing.T) {
	menu := strings.Join(flattenButtons(menus.SubscriptionModelMenu("qoder")), "\n")
	for _, want := range []string{"Qwen3.8-Flash", "Qwen3.8-Max", "Auto"} {
		if !strings.Contains(menu, want) {
			t.Errorf("the Qoder model list does not offer %q:\n%s", want, menu)
		}
	}
	for _, foreign := range []string{"grok", "gpt-", "Gemini", "Claude", "DeepSeek-V3", "Sonnet", "Opus"} {
		if strings.Contains(menu, foreign) {
			t.Errorf("the Qoder model list contains %q:\n%s", foreign, menu)
		}
	}
	if !strings.Contains(menu, "provider:qoder:model:") {
		t.Errorf("Qoder model buttons do not route back through Qoder:\n%s", menu)
	}
}

// Qoder routes resolve like every other switchable provider.
func TestQoderCallbackShapes(t *testing.T) {
	for data, kind := range map[string]providerRouteKind{
		"provider:qoder":            providerRouteActivate,
		"provider:qoder:models":     providerRouteModels,
		"provider:qoder:relogin":    providerRouteRelogin,
		"provider:qoder:model:Auto": providerRoutePickModel,
	} {
		provider, got, _ := routeProviderCallback(data)
		if provider != "qoder" || got != kind {
			t.Errorf("%q routed to (%q, %v); the button is dead", data, provider, got)
		}
	}
}
