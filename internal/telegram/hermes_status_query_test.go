package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ceraa/harness/llm"
	"ceraa/internal/hermesauth"
)

// A status query must not change the active provider.
//
// handleHermesStatus walked every Hermes flow on the host and activated the
// provider on the first that had succeeded. So typing /hermes-status switched the
// model answering underneath the conversation, with nothing in the transcript to
// explain it. The sign-in watchers already refuse an activation whose switch was
// superseded, and this path never went through them at all.
//
// handleCodexStatus has always been only a query, so the shape being restored is
// not a new one.
func TestHermesStatusDoesNotChangeTheActiveProvider(t *testing.T) {
	isolateState(t)
	seen := captureHermesNotify(t)

	if err := defaultState.setProvider(llm.XAISubscription); err != nil {
		t.Fatalf("select xAI: %v", err)
	}
	before := defaultState.provider()

	handleHermesStatus("test-token", 1)

	if after := defaultState.provider(); after != before {
		t.Errorf("/hermes-status changed the active provider from %q to %q", before, after)
	}
	if len(*seen) == 0 {
		t.Error("/hermes-status said nothing at all; a query must still answer")
	}
}

// The credential is the answer, not a resident flow object.
//
// Flows are transient: sign in yesterday, ask today, and no flow exists. The old
// code reported "no completed Hermes login" for a credential that was sitting
// there working, which is a status reply that is wrong rather than merely
// unhelpful.
func TestHermesStatusReportsTheStoredCredential(t *testing.T) {
	isolateState(t)
	seen := captureHermesNotify(t)

	// No flows exist at all, which is the state a returning user is in.
	handleHermesStatus("test-token", 1)

	text := strings.Join(*seen, "\n")
	if strings.Contains(text, "No completed Hermes login") {
		t.Errorf("/hermes-status still reports on flows rather than credentials:\n%s", text)
	}
	if !strings.Contains(text, "/hermes-login") {
		t.Errorf("an unsigned-in status reply should say how to sign in:\n%s", text)
	}
}

// The Check button on a login message is a second route to the same activation,
// and it skipped the guard the watcher applies.
//
// A user signs in, switches to another provider while the browser approval is
// pending, then taps Check. The watcher would have refused that switch; Check
// performed it, because it activated on a bare status read. The owner carries the
// two facts needed to decide, so the tap consults them like every other path.
func TestHermesCheckButtonDoesNotReactivateASupersededSwitch(t *testing.T) {
	isolateState(t)
	beginCapture(t)
	t.Setenv("HERMES_HOME", t.TempDir())

	manager := stubHermesManager(t)
	flow := waitForHermesSuccess(t, manager)

	if err := defaultState.setProvider(llm.XAISubscription); err != nil {
		t.Fatalf("select xAI: %v", err)
	}
	// The login started while Hermes was active and asked to switch; xAI was
	// chosen while the approval was pending.
	trackHermesFlow(flow.ID, 1, true, llm.HermesSubscription)
	defer clearHermesFlow(flow.ID)

	tap(t, "hermes:check:"+flow.ID)

	if after := defaultState.provider(); after != llm.XAISubscription {
		t.Errorf("tapping Check switched the provider to %q, over the xAI the user had chosen since", after)
	}
}

// The other half: the button must still work when the switch is still wanted.
// Without this a fix that simply refused every activation would pass the test
// above and be wrong, which is the failure mode of the guard's own regression.
func TestHermesCheckButtonStillSwitchesWhenTheSwitchIsWanted(t *testing.T) {
	isolateState(t)
	beginCapture(t)
	t.Setenv("HERMES_HOME", t.TempDir())

	manager := stubHermesManager(t)
	flow := waitForHermesSuccess(t, manager)

	if err := defaultState.setProvider(llm.HermesSubscription); err != nil {
		t.Fatalf("select Hermes: %v", err)
	}
	// Still Hermes, so the sign-in's requested switch is still the current choice.
	trackHermesFlow(flow.ID, 1, true, llm.HermesSubscription)
	defer clearHermesFlow(flow.ID)

	tap(t, "hermes:check:"+flow.ID)

	if after := defaultState.provider(); after != llm.HermesSubscription {
		t.Errorf("tapping Check did not switch to Hermes, provider is %q", after)
	}
}

// An unowned flow is reported and not switched.
//
// The owner is cleared once the watcher has delivered, so a Check pressed after
// that finds nothing. The decision was already made, in the one place that makes
// it, and repeating it here would be a second unguarded copy of that decision.
func TestHermesCheckButtonIsInertOnceTheOwnerIsGone(t *testing.T) {
	isolateState(t)
	beginCapture(t)
	t.Setenv("HERMES_HOME", t.TempDir())

	manager := stubHermesManager(t)
	flow := waitForHermesSuccess(t, manager)

	if err := defaultState.setProvider(llm.XAISubscription); err != nil {
		t.Fatalf("select xAI: %v", err)
	}
	trackHermesFlow(flow.ID, 1, true, llm.HermesSubscription)
	clearHermesFlow(flow.ID)

	tap(t, "hermes:check:"+flow.ID)

	if after := defaultState.provider(); after != llm.XAISubscription {
		t.Errorf("tapping Check switched the provider to %q with no owner to authorise it", after)
	}
}

// The guard is shared, so the regression is asserted for every provider that has
// one rather than for Hermes alone. A future provider added without the guard
// would otherwise be found only by using it.
func TestEverySubscriptionProviderGatesItsActivation(t *testing.T) {
	for _, provider := range []string{"Codex", "Hermes", "xAI", "MiniMax", "Cline"} {
		// A sign-in that did not ask to switch is never switched.
		if flowActivationStillWanted(provider, false, "anything") {
			t.Errorf("%s: a sign-in that did not ask to switch still switched", provider)
		}
		// A sign-in whose switch was superseded is not applied late.
		if flowActivationStillWanted(provider, true, llm.HermesSubscription) {
			t.Errorf("%s: a superseded switch was still applied", provider)
		}
	}
}

// stubHermesManager runs a real device flow against a TLS stub that grants it, and
// installs it as the live manager.
//
// The status bug could only be demonstrated with a flow that has actually
// succeeded: the old code activated on the first succeeded flow it found, and a
// fixture with no flows walks past the branch entirely and asserts nothing. So the
// flow is driven end to end rather than faked - the token endpoint requires HTTPS,
// which is why this is a TLS server rather than the plain one used elsewhere.
func stubHermesManager(t *testing.T) *hermesauth.LoginManager {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/oauth/device/code":
			_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"UC-1234","expires_in":600,"interval":1,"verification_uri":"https://portal.example/device"}`))
		case "/api/oauth/token":
			_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expires_in":3600}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	manager := hermesauth.NewLoginManager(&hermesauth.Store{
		Home: t.TempDir(), PortalURL: srv.URL, HTTPClient: srv.Client(),
	})
	previous := hermesAuthMgr
	hermesAuthMgr = manager
	t.Cleanup(func() { hermesAuthMgr = previous })
	return manager
}

// waitForHermesSuccess polls the flow to its terminal state, so the caller is
// holding a credential that really was granted.
func waitForHermesSuccess(t *testing.T, manager *hermesauth.LoginManager) *hermesauth.LoginFlow {
	t.Helper()
	flow, err := manager.Start(context.Background(), "nous")
	if err != nil {
		t.Fatalf("start login: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if status, _, _ := flow.StatusSnapshot(); status == "succeeded" {
			return flow
		}
		if status, _, _ := flow.StatusSnapshot(); status == "failed" || status == "canceled" {
			t.Fatalf("the stub flow did not succeed, it %s", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the stub flow never reached a terminal state")
	return nil
}

// The regression, with a flow that genuinely succeeded.
//
// handleHermesStatus walked every Hermes flow on the host and activated on the
// first that had succeeded. So typing /hermes-status switched the model answering
// underneath the conversation, with nothing in the transcript to explain it. The
// sign-in watchers already refuse an activation whose switch was superseded, and
// this path never went through them.
func TestHermesStatusDoesNotActivateOnASucceededFlow(t *testing.T) {
	isolateState(t)
	captureHermesNotify(t)
	t.Setenv("HERMES_HOME", t.TempDir()) // no stored credential behind the manager

	manager := stubHermesManager(t)
	flow := waitForHermesSuccess(t, manager)

	if err := defaultState.setProvider(llm.XAISubscription); err != nil {
		t.Fatalf("select xAI: %v", err)
	}

	handleHermesStatus("test-token", 1)

	if after := defaultState.provider(); after != llm.XAISubscription {
		t.Errorf("/hermes-status activated %q on the back of a completed login (flow %s) "+
			"while xAI was selected", after, flow.ID)
	}
}
