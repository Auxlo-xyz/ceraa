package hermesauth

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ceraa/internal/secret"
	"time"
)

// This drives the real polling loop through a TLS test server.
//
// The earlier completion test called finish() directly, so it passed even with
// every finish() call removed from the poll loop. The calls in the loop are the
// part that matters: a login that completes in production is reported by exactly
// one of them, and there are seven terminal paths.
func TestPollingLoopReportsSuccess(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/oauth/device/code"):
			_, _ = w.Write([]byte(`{"device_code":"dev-1","user_code":"USER-1","verification_uri":"https://portal.example/verify","expires_in":600,"interval":1}`))
		case strings.HasSuffix(r.URL.Path, "/api/oauth/token"):
			mu.Lock()
			polls++
			first := polls == 1
			mu.Unlock()
			if first {
				// The normal "user has not approved yet" answer. It has to be a
				// non-2xx, because the client only reads error codes from a failing
				// status; a 200 with an error body reads as a malformed success.
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"acc-1","refresh_token":"ref-1","token_type":"Bearer","expires_in":3600,"inference_base_url":"https://inference-api.nousresearch.com/v1"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	// The credential lands in the encrypted secret store, which is initialised at
	// startup in production and nowhere else, so a test that exercises the real
	// save path has to initialise it too. Without this the poll "succeeds" and then
	// fails to persist, which is a genuinely different outcome.
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}

	home := t.TempDir()
	store := NewStore()
	store.Home = home
	store.PortalURL = srv.URL
	store.HTTPClient = srv.Client()

	manager := NewLoginManager(store)
	var got []TerminalState
	manager.SetOnComplete(func(state TerminalState) {
		mu.Lock()
		got = append(got, state)
		mu.Unlock()
	})

	flow, err := manager.Start(t.Context(), "nous")
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	deadline := time.After(20 * time.Second)
	for {
		mu.Lock()
		done := len(got) > 0
		mu.Unlock()
		if done {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the poll loop finished without reporting a terminal state")
		case <-time.After(50 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("reported %d terminal states, want 1", len(got))
	}
	if got[0].Status != StatusSucceeded {
		t.Fatalf("status = %q (%s), want %q", got[0].Status, got[0].Message, StatusSucceeded)
	}
	if got[0].FlowID != flow.ID {
		t.Errorf("state carried flow %q, want %q", got[0].FlowID, flow.ID)
	}

	// The credential has to be readable back, or activation has nothing to use.
	snapshot, err := store.Load()
	if err != nil {
		t.Fatalf("load after login: %v", err)
	}
	credential, err := resolveFromSnapshot(snapshot, "nous")
	if err != nil {
		t.Fatalf("the stored login does not resolve: %v", err)
	}
	if credential.AccessToken == "" {
		t.Error("the stored credential has no access token")
	}
	if credential.BaseURL != "https://inference-api.nousresearch.com/v1" {
		t.Errorf("inference base URL = %q, want the portal's", credential.BaseURL)
	}
}

// The cancel path has to report too. It is the one terminal state the user
// triggered themselves, so it is the easiest to leave unwired on the reasoning
// that the cancelling button already edited its own message.
func TestPollingLoopReportsCancel(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/api/oauth/device/code") {
			_, _ = w.Write([]byte(`{"device_code":"dev-c","user_code":"USER-C","verification_uri":"https://portal.example/verify","expires_in":600,"interval":1}`))
			return
		}
		// Never approved, so the flow stays live until it is cancelled.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}
	store := NewStore()
	store.Home = t.TempDir()
	store.PortalURL = srv.URL
	store.HTTPClient = srv.Client()

	manager := NewLoginManager(store)
	var mu sync.Mutex
	var got []TerminalState
	manager.SetOnComplete(func(state TerminalState) {
		mu.Lock()
		got = append(got, state)
		mu.Unlock()
	})

	flow, err := manager.Start(t.Context(), "nous")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Let the poll loop get going, so the cancel is observed by the running loop
	// rather than by Cancel's own bookkeeping.
	time.Sleep(150 * time.Millisecond)
	if err := manager.Cancel(flow.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	deadline := time.After(20 * time.Second)
	for {
		mu.Lock()
		done := len(got) > 0
		mu.Unlock()
		if done {
			break
		}
		select {
		case <-deadline:
			t.Fatal("a cancelled login reported no terminal state")
		case <-time.After(50 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("reported %d terminal states, want 1", len(got))
	}
	if got[0].Status != StatusCanceled {
		t.Errorf("status = %q, want %q", got[0].Status, StatusCanceled)
	}
}

func TestPollingLoopReportsHardFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/api/oauth/device/code") {
			_, _ = w.Write([]byte(`{"device_code":"dev-f","user_code":"USER-F","verification_uri":"https://portal.example/verify","expires_in":600,"interval":1}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"access_denied","error_description":"user refused"}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	store.Home = t.TempDir()
	store.PortalURL = srv.URL
	store.HTTPClient = srv.Client()
	manager := NewLoginManager(store)
	var mu sync.Mutex
	var got []TerminalState
	manager.SetOnComplete(func(state TerminalState) {
		mu.Lock()
		got = append(got, state)
		mu.Unlock()
	})
	if _, err := manager.Start(t.Context(), "nous"); err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.After(20 * time.Second)
	for {
		mu.Lock()
		done := len(got) > 0
		mu.Unlock()
		if done {
			break
		}
		select {
		case <-deadline:
			t.Fatal("a refused login reported no terminal state")
		case <-time.After(50 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if got[0].Status != StatusFailed {
		t.Errorf("status = %q, want %q", got[0].Status, StatusFailed)
	}
}

// A malformed token response is a plain error, not a device-code error, so it
// takes the other failure branch. Both branches have to report, and they are easy
// to confuse because only one of them looks like the "expected" failure.
func TestPollingLoopReportsMalformedTokenResponse(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/api/oauth/device/code") {
			_, _ = w.Write([]byte(`{"device_code":"dev-m","user_code":"USER-M","verification_uri":"https://portal.example/verify","expires_in":600,"interval":1}`))
			return
		}
		// 200 with a body that is not JSON: a success status with unusable content.
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}
	store := NewStore()
	store.Home = t.TempDir()
	store.PortalURL = srv.URL
	store.HTTPClient = srv.Client()

	manager := NewLoginManager(store)
	var mu sync.Mutex
	var got []TerminalState
	manager.SetOnComplete(func(state TerminalState) {
		mu.Lock()
		got = append(got, state)
		mu.Unlock()
	})
	if _, err := manager.Start(t.Context(), "nous"); err != nil {
		t.Fatalf("start: %v", err)
	}

	deadline := time.After(20 * time.Second)
	for {
		mu.Lock()
		done := len(got) > 0
		mu.Unlock()
		if done {
			break
		}
		select {
		case <-deadline:
			t.Fatal("a malformed response reported no terminal state")
		case <-time.After(50 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("reported %d terminal states, want 1", len(got))
	}
	if got[0].Status != StatusFailed {
		t.Errorf("status = %q, want %q", got[0].Status, StatusFailed)
	}
}
