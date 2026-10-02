package event

import (
	"testing"
	"time"
)

// recv takes one event or fails the test if none arrives.
func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case one, open := <-ch:
		if !open {
			t.Fatal("subscription channel closed")
		}
		return one
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for an event")
		return Event{}
	}
}

// TestWildcardReceivesEveryEvent covers the observer that has not been told
// which session matters yet, which is the position a dashboard is in when it
// first opens.
func TestWildcardReceivesEveryEvent(t *testing.T) {
	bus := New()
	all := bus.Subscribe(Wildcard, 8)
	bus.Publish(Event{Type: TypePermission, SessionID: "ses-a"})

	got := recv(t, all)
	if got.Type != TypePermission || got.SessionID != "ses-a" {
		t.Fatalf("got type=%q session=%q", got.Type, got.SessionID)
	}
	if got.Time.IsZero() {
		t.Error("published event carries no timestamp")
	}
}

// TestWildcardDoesNotDoubleDeliver pins that a subscriber listening for one
// session gets one copy, not one from its own key and another from the
// wildcard.
func TestWildcardDoesNotDoubleDeliver(t *testing.T) {
	bus := New()
	one := bus.Subscribe("ses-a", 8)
	bus.Publish(Event{Type: TypeQuestion, SessionID: "ses-a"})

	if got := recv(t, one); got.Type != TypeQuestion {
		t.Fatalf("got type=%q", got.Type)
	}
	select {
	case extra := <-one:
		t.Fatalf("received a duplicate: %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestUnsubscribeStopsDelivery keeps a closed stream from being handed events
// after its reader has gone.
func TestUnsubscribeStopsDelivery(t *testing.T) {
	bus := New()
	ch := bus.Subscribe(Wildcard, 8)
	bus.Unsubscribe(Wildcard, ch)

	if _, open := <-ch; open {
		t.Fatal("channel still open after Unsubscribe")
	}
	bus.Publish(Event{Type: TypeComplete, SessionID: "ses-a"})
	// Nothing should have been queued, but the assertion that matters is the
	// closed channel above; a send on a closed channel would have panicked.
}
