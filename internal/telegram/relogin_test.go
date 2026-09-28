package telegram

import (
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/config"
	"ceraa/internal/telegram/menus"
)

// "Sign in again" must not share a callback with "Switch to".
//
// The entry point activates when a credential exists, so the shared callback made
// re-authentication unreachable: tapping it re-activated the provider, and
// removing the provider did not help because removal clears the profile while the
// credential stays in the encrypted store.
func TestSignInAgainHasItsOwnCallback(t *testing.T) {
	for _, providerID := range []string{"minimax", "hermes", "xai", "cline"} {
		// Both branches that offer the button, because either one could alias it.
		for _, tc := range []struct{ signedIn, active bool }{{true, true}, {true, false}} {
			markup := menus.SubscriptionMenuForTest("Test", providerID, tc.signedIn, tc.active)
			raw := callbacksIn(t, markup)
			joined := strings.Join(raw, "\x00")
			if !strings.Contains(joined, "provider:"+providerID+":relogin") {
				t.Errorf("%s (signedIn=%v active=%v): no distinct re-login callback in %v",
					providerID, tc.signedIn, tc.active, raw)
			}
		}
	}
}

// Keeping the active model must look at the model.
//
// values() returns (model, agent) and every provider's helper read the second
// value, so the check compared the agent mode against a list of model ids and
// could never fire. That is how a profile ended up with no model at all.
func TestKeepingTheActiveModelReadsTheModelNotTheAgent(t *testing.T) {
	restore := stubBothState(t)
	t.Cleanup(restore)

	if err := providers.setAgent("build"); err != nil {
		t.Fatal(err)
	}
	if err := providers.setModel(llm.DefaultMiniMaxModel); err != nil {
		t.Fatal(err)
	}
	model, agent := providers.modelAndAgent()
	if model != llm.DefaultMiniMaxModel {
		t.Errorf("model = %q, want %q", model, llm.DefaultMiniMaxModel)
	}
	if agent != "build" {
		t.Errorf("agent = %q, want build", agent)
	}

	// With the model set, the helper must keep it rather than fall back.
	chosen := minimaxModelForAccount("")
	if chosen != llm.DefaultMiniMaxModel {
		t.Errorf("minimaxModelForAccount = %q, want the active model", chosen)
	}
	// And with the model belonging to another provider, it must fall back to this
	// provider's default rather than carrying a foreign model across.
	if err := providers.setModel("grok-4.6"); err != nil {
		t.Fatal(err)
	}
	chosen = minimaxModelForAccount("")
	if chosen != llm.DefaultMiniMaxModel {
		t.Errorf("minimaxModelForAccount = %q, want the MiniMax default", chosen)
	}
}

// An unusable profile is refused at the point of activation, not discovered by the
// endpoint.
func TestActivationRefusesAnEmptyModel(t *testing.T) {
	if _, err := guardModel("minimax", ""); err == nil {
		t.Error("an empty model was accepted; the request would go out with no model id")
	}
	if _, err := guardModel("minimax", "   "); err == nil {
		t.Error("a blank model was accepted")
	}
	if got, err := guardModel("minimax", "  MiniMax-M3  "); err != nil || got != "MiniMax-M3" {
		t.Errorf("guardModel = %q, %v", got, err)
	}
}

// The stored profile must never be left without a model.
func TestMinimaxProfileAlwaysCarriesAModel(t *testing.T) {
	restore := stubBothState(t)
	t.Cleanup(restore)
	_ = providers.setAgent("build")

	profile := config.ProviderProfile{
		URL:          llm.MiniMaxBaseURL,
		Model:        llm.DefaultMiniMaxModel,
		Subscription: llm.MiniMaxSubscription,
	}
	if err := providers.setProfile("minimax", profile); err != nil {
		t.Fatal(err)
	}
	if err := providers.setActive("minimax"); err != nil {
		t.Fatal(err)
	}
	_, stored, ok := providers.activeProfile()
	if !ok {
		t.Fatal("no active profile after activation")
	}
	if strings.TrimSpace(stored.Model) == "" {
		t.Error("the activated profile has no model")
	}
}
