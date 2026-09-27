package hermesauth

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A device login has to finish on its own. The user approves the code in a
// browser and never touches the bot again, so without a completion callback the
// credential lands silently and the provider never activates.
func TestTerminalStatesReachTheCompletionSubscriber(t *testing.T) {
	cases := []struct {
		name   string
		status func(*LoginFlow)
		want   string
	}{
		{"expired", func(f *LoginFlow) { f.setFailed("Authorization expired") }, StatusFailed},
		{"canceled", func(f *LoginFlow) { f.setFailed("Login cancelled") }, StatusCanceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewLoginManager(&Store{Home: t.TempDir()})
			var mu sync.Mutex
			var got []TerminalState
			m.SetOnComplete(func(state TerminalState) {
				mu.Lock()
				got = append(got, state)
				mu.Unlock()
			})
			flow := &LoginFlow{ID: "flow-" + tc.name, Provider: "nous"}
			tc.status(flow)
			m.finish(flow, tc.want, flow.Message)

			mu.Lock()
			defer mu.Unlock()
			if len(got) != 1 {
				t.Fatalf("got %d terminal states, want 1", len(got))
			}
			if got[0].Status != tc.want {
				t.Errorf("status = %q, want %q", got[0].Status, tc.want)
			}
			if got[0].FlowID != flow.ID || got[0].Provider != "nous" {
				t.Errorf("state lost its identity: %+v", got[0])
			}
		})
	}
}

// The callback runs outside the manager lock, so a subscriber that calls back
// into the manager must not deadlock. Codex needed the same guarantee and it is
// the reason finish() takes the lock only to read the function.
func TestCompletionSubscriberCanUseTheManager(t *testing.T) {
	m := NewLoginManager(&Store{Home: t.TempDir()})
	done := make(chan struct{})
	m.SetOnComplete(func(state TerminalState) {
		// Touching the manager from inside the callback is the deadlock case.
		m.ListFlows()
		close(done)
	})
	flow := &LoginFlow{ID: "f1", Provider: "nous"}
	m.finish(flow, StatusSucceeded, "ok")

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the subscriber deadlocked against the manager lock")
	}
}

// No subscriber must not panic, and must not be a nil-function call.
func TestFinishWithoutSubscriberIsSafe(t *testing.T) {
	m := NewLoginManager(&Store{Home: t.TempDir()})
	flow := &LoginFlow{ID: "f2", Provider: "nous"}
	m.finish(flow, StatusSucceeded, "ok")
	m.finish(flow, StatusFailed, "later")
}

// Start still refuses non-Nous providers. A subscriber must not change which
// providers can start a native login.
func TestStartRefusesUnsupportedProvider(t *testing.T) {
	m := NewLoginManager(&Store{Home: t.TempDir()})
	if _, err := m.Start(context.Background(), "xai-oauth"); err == nil {
		t.Fatal("expected an error for a provider without a native login")
	}
}
