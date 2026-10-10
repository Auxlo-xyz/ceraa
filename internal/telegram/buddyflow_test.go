package telegram

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"ceraa/internal/buddyauth"
	"ceraa/internal/telegram/menus"
)

var errBuddyBeginTest = errors.New("test begin failure")

type buddyNotifications struct {
	mu   sync.Mutex
	seen []string
}

func (n *buddyNotifications) snapshot() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.seen...)
}

func captureBuddyNotify(t *testing.T) *buddyNotifications {
	t.Helper()
	seen := &buddyNotifications{}
	original := buddyNotify
	buddyNotify = func(token string, chatID int64, text string, kb map[string]any) {
		seen.mu.Lock()
		seen.seen = append(seen.seen, text)
		seen.mu.Unlock()
	}
	t.Cleanup(func() { buddyNotify = original })
	return seen
}

// The button existing and being handled are separate facts, for both regions.
func TestProviderBuddyCallbacksReachTheEntryPoint(t *testing.T) {
	type call struct {
		provider string
		data     string
	}
	var mu sync.Mutex
	var got []string
	original := buddyEntryPoint
	buddyEntryPoint = func(token string, chatID int64, product buddyauth.Product) {
		mu.Lock()
		got = append(got, product.ID)
		mu.Unlock()
	}
	t.Cleanup(func() { buddyEntryPoint = original })

	for _, tc := range []call{
		{"buddy", "provider:buddy"},
		{"workbuddy", "provider:workbuddy"},
	} {
		var query callbackQuery
		if err := json.Unmarshal([]byte(`{"id":"1","data":"`+tc.data+`","message":{"chat":{"id":5151},"message_id":5}}`), &query); err != nil {
			t.Fatalf("callback: %v", err)
		}
		handleCallback("test-token", &query)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "buddy") || !strings.Contains(joined, "workbuddy") {
		t.Errorf("entry reached for %v; a button is dead", got)
	}
}

// A login that cannot start tells the chat instead of going quiet.
func TestBuddyLoginStartFailureNotifies(t *testing.T) {
	seen := captureBuddyNotify(t)

	originalBegin := buddyBeginLogin
	buddyBeginLogin = func(*buddyauth.Manager) (*buddyauth.Flow, error) {
		return nil, errBuddyBeginTest
	}
	t.Cleanup(func() { buddyBeginLogin = originalBegin })

	var query callbackQuery
	if err := json.Unmarshal([]byte(`{"id":"1","data":"provider:buddy:relogin","message":{"chat":{"id":5151},"message_id":5}}`), &query); err != nil {
		t.Fatalf("callback: %v", err)
	}
	handleCallback("test-token", &query)

	joined := strings.Join(seen.snapshot(), "\n")
	if !strings.Contains(joined, "Failed to start buddy login") {
		t.Errorf("no failure report; notified:\n%v", seen.snapshot())
	}
}

// A finished login with an owner switches provider when that is still wanted.
func TestBuddyLoginSuccessActivates(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	seen := captureBuddyNotify(t)
	if err := providers.setActive("opencode"); err != nil {
		t.Fatal(err)
	}

	trackBuddyFlow("flow-1", 5151, true, "opencode", "buddy")
	deliverBuddyTerminal("test-token", "flow-1", true, "")

	if got := providers.active(); got != "buddy" {
		t.Errorf("active = %q after a successful owned login; want buddy", got)
	}
	if joined := strings.Join(seen.snapshot(), "\n"); !strings.Contains(joined, "buddy provider activated") {
		t.Errorf("no activation confirmation; notified:\n%v", seen.snapshot())
	}
	if _, ok := buddyFlowOwnerOf("flow-1"); ok {
		t.Error("the flow was not cleared after delivery")
	}
}

// A failed login tells the chat plainly instead of going quiet.
func TestBuddyLoginFailureNotifies(t *testing.T) {
	seen := captureBuddyNotify(t)

	trackBuddyFlow("flow-2", 5151, true, "opencode", "workbuddy")
	deliverBuddyTerminal("test-token", "flow-2", false, "the user declined")

	joined := strings.Join(seen.snapshot(), "\n")
	if !strings.Contains(joined, "did not complete") {
		t.Errorf("no failure report; notified:\n%v", seen.snapshot())
	}
}

// The model lists are scoped per region: domestic ids on buddy, intl aliases
// on workbuddy, each routed back through its own provider.
func TestBuddyModelMenusAreScopedPerRegion(t *testing.T) {
	buddy := strings.Join(flattenButtons(menus.SubscriptionModelMenu("buddy")), "\n")
	workbuddy := strings.Join(flattenButtons(menus.SubscriptionModelMenu("workbuddy")), "\n")
	if !strings.Contains(buddy, "deepseek-v4.1-flash") {
		t.Errorf("the buddy list misses domestic ids:\n%s", buddy)
	}
	if !strings.Contains(workbuddy, "default-model") || !strings.Contains(workbuddy, "gpt-6-astra") {
		t.Errorf("the workbuddy list misses intl ids:\n%s", workbuddy)
	}
	if strings.Contains(buddy, "gpt-6-astra") {
		t.Errorf("the buddy list contains intl models:\n%s", buddy)
	}
	if !strings.Contains(buddy, "provider:buddy:model:") || !strings.Contains(workbuddy, "provider:workbuddy:model:") {
		t.Errorf("model buttons do not route back through their provider")
	}
}

// Buddy routes resolve like every other switchable provider.
func TestBuddyCallbackShapes(t *testing.T) {
	for data, want := range map[string]struct {
		provider string
		kind     providerRouteKind
	}{
		"provider:buddy":             {"buddy", providerRouteActivate},
		"provider:buddy:models":      {"buddy", providerRouteModels},
		"provider:buddy:relogin":     {"buddy", providerRouteRelogin},
		"provider:workbuddy":         {"workbuddy", providerRouteActivate},
		"provider:workbuddy:models":  {"workbuddy", providerRouteModels},
		"provider:workbuddy:relogin": {"workbuddy", providerRouteRelogin},
	} {
		provider, got, _ := routeProviderCallback(data)
		if provider != want.provider || got != want.kind {
			t.Errorf("%q routed to (%q, %v); the button is dead", data, provider, got)
		}
	}
}
