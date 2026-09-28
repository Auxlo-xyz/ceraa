package telegram

import (
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/oauthdevice"
)

// A login flow id, in the exact shape oauthdevice mints: provider, unix seconds,
// sequence.
const flowIDShape = "minimax-1790571540-1"

// The regression: every activation helper returned (flowID, model) and every
// caller destructured it as (model, _), so activating a provider wrote the login
// flow id into its profile. It went out as the model id, into the model-identity
// block the model is told to trust, and into the pricing lookup, which reported
// model_not_found and priced nothing.
func TestActivationStoresAModelNotTheLoginFlowID(t *testing.T) {
	restore := stubBothState(t)
	t.Cleanup(restore)
	if err := providers.setAgent("build"); err != nil {
		t.Fatal(err)
	}

	helpers := map[string]func(string) string{
		"minimax": minimaxModelForAccount,
		"cline":   clineModelForAccount,
		"hermes":  hermesModelForAccount,
		"xai":     xaiModelForAccount,
	}
	for provider, helper := range helpers {
		flowID := provider + "-1790571540-1"
		chosen := helper(flowID)
		if chosen == flowID {
			t.Errorf("%s: activation would store the flow id %q as the model", provider, flowID)
		}
		if chosen == "" {
			t.Errorf("%s: activation resolved to no model at all", provider)
		}
		if !llm.IsSubscriptionModel(provider, chosen) {
			t.Errorf("%s: %q is not a model this provider serves", provider, chosen)
		}
	}
}

// guardModel is the last thing between a bad model and a live profile, and it
// used to check only for the empty string. Nothing verified the provider served
// the value, so the flow id above passed straight through to production.
func TestGuardModelRejectsAModelTheProviderDoesNotServe(t *testing.T) {
	for _, provider := range []string{"minimax", "cline", "hermes", "xai"} {
		got, err := guardModel(provider, flowIDShape)
		if err != nil {
			t.Errorf("%s: guardModel(%q) errored: %v", provider, flowIDShape, err)
			continue
		}
		if got == flowIDShape {
			t.Errorf("%s: guardModel passed the flow id through as the model", provider)
		}
		if got != llm.DefaultModelFor(provider) {
			t.Errorf("%s: guardModel = %q, want the provider default %q", provider, got, llm.DefaultModelFor(provider))
		}
		if !llm.IsSubscriptionModel(provider, got) {
			t.Errorf("%s: guardModel returned %q, which the provider does not serve", provider, got)
		}
	}
}

// A model the provider does serve must survive the guard untouched, or a correct
// choice from the model menu would be discarded on activation.
func TestGuardModelKeepsAModelTheProviderServes(t *testing.T) {
	for _, provider := range []string{"minimax", "cline", "hermes", "xai"} {
		models := llm.SubscriptionModels(provider)
		if len(models) == 0 {
			t.Fatalf("%s: no models registered, so this test proves nothing", provider)
		}
		for _, want := range models {
			got, err := guardModel(provider, want)
			if err != nil {
				t.Errorf("%s: guardModel(%q) errored: %v", provider, want, err)
				continue
			}
			if got != want {
				t.Errorf("%s: guardModel(%q) = %q, want it unchanged", provider, want, got)
			}
		}
	}
}

// Absent stays an error. A wrong model is recoverable; a missing one is a
// profile the operator has to fix, and substituting a default would hide it.
func TestGuardModelStillRefusesAnAbsentModel(t *testing.T) {
	for _, provider := range []string{"minimax", "cline", "hermes", "xai"} {
		for _, blank := range []string{"", "   "} {
			if got, err := guardModel(provider, blank); err == nil {
				t.Errorf("%s: guardModel(%q) = %q, want an error", provider, blank, got)
			}
		}
	}
}

// A provider that is not a subscription has no default to fall back to, so it
// must fail loudly rather than activate with a model nobody checked.
func TestGuardModelUnknownProviderFailsLoudly(t *testing.T) {
	if got, err := guardModel("not-a-provider", flowIDShape); err == nil {
		t.Errorf("guardModel accepted an unknown provider and returned %q", got)
	}
}

// The flow id is only recognisable as junk because it is not in the served list,
// which is what the guard above depends on.
func TestAFlowIDIsNotAModel(t *testing.T) {
	if oauthdevice.GrantTypeDeviceCode == "" {
		t.Fatal("grant type constant missing")
	}
	if llm.IsSubscriptionModel("minimax", flowIDShape) {
		t.Error("a login flow id is being served as a model id")
	}
}
