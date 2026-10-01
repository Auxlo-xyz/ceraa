package telegram

import (
	"encoding/json"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/config"
)

// stubState points the state at a temp dir, following the pattern the Codex flow
// tests already use, so a profile write cannot touch the real one.
func stubState(t *testing.T) func() {
	return stubBothState(t)
}

// Choosing a model must write the profile, or the picker is decorative.
func TestSelectingAModelWritesTheProfile(t *testing.T) {
	restore := stubState(t)
	defer restore()

	selectSubscriptionModel("test-token", 1, "cline", llm.DefaultClineModel)

	profile, ok := currentProfile("cline")
	if !ok {
		t.Fatal("no profile was written for the chosen model")
	}
	if profile.Model != llm.DefaultClineModel {
		t.Errorf("profile model = %q, want %q", profile.Model, llm.DefaultClineModel)
	}
	if profile.Subscription != llm.ClineSubscription {
		t.Errorf("the subscription marker was lost: %q", profile.Subscription)
	}
}

// Choosing a model must not discard the rest of the profile. Rebuilding it from
// scratch on every choice would drop the URL and context limit, and the provider
// would stop working.
func TestChoosingAModelKeepsTheRestOfTheProfile(t *testing.T) {
	restore := stubState(t)
	defer restore()

	original := config.ProviderProfile{
		URL:          "https://api.cline.bot/api/v1",
		Model:        "something-else",
		ContextLimit: 123456,
	}
	if err := defaultState.setProviderProfile("cline", original); err != nil {
		t.Fatalf("seed: %v", err)
	}

	selectSubscriptionModel("test-token", 1, "cline", llm.DefaultClineModel)

	profile, ok := currentProfile("cline")
	if !ok {
		t.Fatal("the profile vanished")
	}
	if profile.URL != original.URL {
		t.Errorf("choosing a model discarded the URL: %q -> %q", original.URL, profile.URL)
	}
	if profile.ContextLimit != original.ContextLimit {
		t.Errorf("choosing a model discarded the context limit: %d -> %d",
			original.ContextLimit, profile.ContextLimit)
	}
	if profile.Model != llm.DefaultClineModel {
		t.Errorf("the model was not changed: %q", profile.Model)
	}
}

// A model from another provider must be refused. A crafted callback must not be
// able to write a profile the endpoint rejects.
func TestSelectingAModelFromAnotherProviderIsRefused(t *testing.T) {
	restore := stubState(t)
	defer restore()

	selectSubscriptionModel("test-token", 1, "cline", "MiniMax-M3")

	profile, ok := currentProfile("cline")
	if ok && profile.Model == "MiniMax-M3" {
		t.Error("a refused model was still written to the profile")
	}
	if _, ok := currentProfile("cline"); ok {
		t.Error("a refused model still wrote a profile")
	}
}

// An unknown provider has to be refused rather than silently accepted.
func TestSelectingAModelForAnUnknownProviderIsRefused(t *testing.T) {
	restore := stubState(t)
	defer restore()

	selectSubscriptionModel("test-token", 1, "not-a-provider", "anything")

	if _, ok := currentProfile("not-a-provider"); ok {
		t.Error("an unknown provider wrote a profile")
	}
}

// The callback has to route to the handler. The button existing and being handled
// are separate facts, and only the first was covered once already this session.
//
// An earlier version of this test called handleCallback and asserted nothing at
// all, so removing the routing passed it: the callback ran, did nothing, and the
// test was happy. It now asserts the profile the callback is supposed to write.
func TestProviderModelCallbackRoutes(t *testing.T) {
	for providerID, marker := range map[string]string{
		"codex": llm.CodexSubscription, "hermes": llm.HermesSubscription,
		"xai": llm.XAISubscription, "minimax": llm.MiniMaxSubscription,
		"cline": llm.ClineSubscription, "opencode": llm.OpenCodeSubscription,
	} {
		t.Run(providerID, func(t *testing.T) {
			restore := stubState(t)
			defer restore()

			want := llm.DefaultModelFor(marker)
			data := "provider:" + providerID + ":model:" + want
			var query callbackQuery
			if err := json.Unmarshal([]byte(`{"id":"1","data":"`+data+`","message":{"chat":{"id":7},"message_id":5}}`), &query); err != nil {
				t.Fatalf("%v", err)
			}
			handleCallback("test-token", &query)

			profile, ok := currentProfile(providerID)
			if !ok {
				t.Fatalf("the callback wrote no profile for %s; the button is dead", providerID)
			}
			if profile.Model != want {
				t.Errorf("profile model = %q, want %q", profile.Model, want)
			}
		})
	}
}

// The model list and the default must agree, or the picker shows a different
// default from the one activation uses.
func TestSubscriptionMarkersMapToEveryProfile(t *testing.T) {
	for providerID, want := range map[string]string{
		"codex": llm.CodexSubscription, "hermes": llm.HermesSubscription,
		"xai": llm.XAISubscription, "minimax": llm.MiniMaxSubscription,
		"cline": llm.ClineSubscription, "opencode": llm.OpenCodeSubscription,
	} {
		if got := subscriptionMarkerFor(providerID); got != want {
			t.Errorf("%s maps to %q, want %q", providerID, got, want)
		}
	}
}
