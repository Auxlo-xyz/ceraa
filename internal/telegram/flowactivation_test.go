package telegram

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ceraa/internal/codexauth"
	"ceraa/internal/hermesauth"
	"ceraa/internal/oauthdevice"
)

// The incident this file exists for: a MiniMax device login was approved, and
// while it was pending the user switched to xAI and was told "xAI/Grok provider
// activated with model grok-4.7". A minute later the hub read
// "MiniMax · active" again. The switch had not failed and neither had the sign-in;
// the sign-in simply finished later and activated, overwriting a choice the user
// had already made and been confirmed on.
//
// The log line that named it was "minimax activate: provider=xai model=MiniMax-M3":
// MiniMax activating while xAI was active is only possible if the decision was
// taken before the xAI switch and applied after it.

// selectProviderForTest points the process-wide selection at id and restores it,
// because the selection is a global the whole package shares.
func selectProviderForTest(t *testing.T, id string) {
	t.Helper()
	previous := providers.active()
	if err := providers.setActive(id); err != nil {
		t.Fatalf("select %s: %v", id, err)
	}
	t.Cleanup(func() {
		if err := providers.setActive(previous); err != nil {
			t.Fatalf("restore %s: %v", previous, err)
		}
	})
}

// stubTelegramAPI keeps the activation confirmation off the network, and records
// the paths it was asked to send so a test can assert the user was told.
func stubTelegramAPI(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	t.Cleanup(server.Close)
	previous := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	t.Cleanup(func() { telegramAPIBase = previous })
	return &calls
}

// The regression, end to end through the completion path rather than through the
// guard alone: drive the exact sequence, and the newer choice has to survive.
func TestASupersededSignInDoesNotOverwriteANewerChoice(t *testing.T) {
	seen := captureMinimaxNotify(t)
	stubTelegramAPI(t)

	// MiniMax is active and the user reaches for its sign-in. The provider is read
	// now, before the login begins, exactly as a production entry point does.
	selectProviderForTest(t, "minimax")
	trackMinimaxFlow("f-race", 77, true, "minimax")
	defer clearMinimaxFlow("f-race")

	// While that is pending, they choose xAI and are confirmed on it.
	selectProviderForTest(t, "xai")

	// Only now does the MiniMax sign-in land.
	deliverMinimaxTerminal("test-token", oauthdevice.Snapshot{
		ID: "f-race", Status: oauthdevice.StatusSucceeded,
	})

	if got := defaultState.provider(); got != "xai" {
		t.Fatalf("active provider = %q, want xai: a sign-in that finished after a deliberate "+
			"switch overwrote it", got)
	}
	if len(*seen) != 1 {
		t.Fatalf("got %d notifications, want 1: the user has to be told the sign-in landed", len(*seen))
	}
	if !strings.Contains((*seen)[0], "signed in") {
		t.Errorf("the outcome did not say the credential is ready: %q", (*seen)[0])
	}
	if !strings.Contains((*seen)[0], "another provider was chosen") {
		t.Errorf("the outcome did not say the switch was superseded, so the choice reads as ignored: %q", (*seen)[0])
	}
}

// The other half: the guard must not turn into a refusal to ever activate. A
// sign-in that completes while its own provider is still the active one switches.
func TestASignInStillSwitchesWhenNothingElseWasChosen(t *testing.T) {
	captureMinimaxNotify(t)
	stubTelegramAPI(t)

	selectProviderForTest(t, "minimax")
	trackMinimaxFlow("f-quiet", 78, true, "minimax")
	defer clearMinimaxFlow("f-quiet")

	deliverMinimaxTerminal("test-token", oauthdevice.Snapshot{
		ID: "f-quiet", Status: oauthdevice.StatusSucceeded,
	})

	if got := defaultState.provider(); got != "minimax" {
		t.Fatalf("active provider = %q, want minimax: a sign-in that was left alone took over anyway", got)
	}
}

// A flow tracked without knowing what was active is the pre-existing behaviour:
// the sign-in asked to switch, so it switches. A conservative reading here would
// silently break activation for any path that under-reports.
func TestAnUntrackedSelectionStillSwitches(t *testing.T) {
	captureMinimaxNotify(t)
	stubTelegramAPI(t)

	selectProviderForTest(t, "xai")
	owner := minimaxFlowOwner{chatID: 79, activate: true, activeAtStart: ""}
	reportMinimaxSignedIn("test-token", owner, "f-blind")

	if got := defaultState.provider(); got != "minimax" {
		t.Fatalf("active provider = %q, want minimax: a sign-in with no recorded selection "+
			"was refused, which is a regression in the opposite direction", got)
	}
}

// Every provider carries the same shape, so the guard is exercised through all
// five completion paths. A fix to one file alone would leave four races in place.
func TestEveryProviderRespectsTheSelectionMadeAfterItsSignInStarted(t *testing.T) {
	cases := []struct {
		provider string
		track    func(string, int64, bool, string)
		clear    func(string)
		deliver  func(string)
		owner    func(string) (string, bool)
	}{
		{
			provider: "minimax",
			track:    trackMinimaxFlow,
			clear:    clearMinimaxFlow,
			deliver: func(id string) {
				deliverMinimaxTerminal("test-token", oauthdevice.Snapshot{ID: id, Status: oauthdevice.StatusSucceeded})
			},
			owner: func(id string) (string, bool) {
				o, ok := minimaxFlowOwnerOf(id)
				return o.activeAtStart, ok
			},
		},
		{
			provider: "xai",
			track:    trackXaiFlow,
			clear:    clearXaiFlow,
			deliver: func(id string) {
				deliverXaiTerminal("test-token", oauthdevice.Snapshot{ID: id, Status: oauthdevice.StatusSucceeded})
			},
			owner: func(id string) (string, bool) {
				o, ok := xaiFlowOwnerOf(id)
				return o.activeAtStart, ok
			},
		},
		{
			provider: "cline",
			track:    trackClineFlow,
			clear:    clearClineFlow,
			deliver: func(id string) {
				deliverClineTerminal("test-token", oauthdevice.Snapshot{ID: id, Status: oauthdevice.StatusSucceeded})
			},
			owner: func(id string) (string, bool) {
				o, ok := clineFlowOwnerOf(id)
				return o.activeAtStart, ok
			},
		},
		{
			provider: "hermes",
			track:    trackHermesFlow,
			clear:    clearHermesFlow,
			deliver: func(id string) {
				deliverHermesTerminal("test-token", hermesauth.TerminalState{
					FlowID: id, Status: hermesauth.StatusSucceeded,
				})
			},
			owner: func(id string) (string, bool) {
				o, ok := hermesFlowOwnerOf(id)
				return o.activeAtStart, ok
			},
		},
		{
			provider: "codex",
			track:    trackCodexFlow,
			clear:    clearCodexFlow,
			deliver: func(id string) {
				deliverCodexTerminal("test-token", codexauth.State{
					FlowID: id, Status: codexauth.StatusSucceeded,
				})
			},
			owner: func(id string) (string, bool) {
				o, ok := codexFlowOwnerOf(id)
				return o.activeAtStart, ok
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			flowID := "f-" + tc.provider
			stubTelegramAPI(t)
			selectProviderForTest(t, tc.provider)
			tc.track(flowID, 90, true, tc.provider)
			defer tc.clear(flowID)

			recorded, ok := tc.owner(flowID)
			if !ok {
				t.Fatalf("%s did not record the flow owner", tc.provider)
			}
			if recorded != tc.provider {
				t.Fatalf("%s recorded the selection at start as %q, want %q: with it empty the "+
					"completion cannot tell a superseded switch from a wanted one", tc.provider, recorded, tc.provider)
			}

			// The user chooses something else while the sign-in is pending.
			selectProviderForTest(t, "default")
			tc.deliver(flowID)

			if got := defaultState.provider(); got != "default" {
				t.Errorf("active provider = %q, want default: a %s sign-in that finished after a "+
					"deliberate switch overwrote it", got, tc.provider)
			}
		})
	}
}

// The guard on its own, so its four cases are pinned independently of the wiring:
// a refusal, the unknown-selection fallback, a match, and a supersession.
func TestTheGuardDecidesFromWhatWasActiveAtTheStart(t *testing.T) {
	cases := []struct {
		name          string
		activate      bool
		activeAtStart string
		current       string
		want          bool
	}{
		{name: "a sign-in that did not ask to switch", activate: false, activeAtStart: "minimax", current: "minimax", want: false},
		{name: "nothing was chosen since", activate: true, activeAtStart: "minimax", current: "minimax", want: true},
		{name: "another provider was chosen since", activate: true, activeAtStart: "minimax", current: "xai", want: false},
		{name: "the selection at start was never recorded", activate: true, activeAtStart: "", current: "xai", want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selectProviderForTest(t, tc.current)
			if got := flowActivationStillWanted("minimax", tc.activate, tc.activeAtStart); got != tc.want {
				t.Errorf("flowActivationStillWanted(%v, %q) with %q active = %v, want %v",
					tc.activate, tc.activeAtStart, tc.current, got, tc.want)
			}
		})
	}
}
