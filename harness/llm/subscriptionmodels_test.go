package llm

import "testing"

// Every subscription provider needs a list, or there is nothing to pick from and
// the provider activates with a hardcoded default nothing can change.
func TestEverySubscriptionProviderHasModels(t *testing.T) {
	for _, subscription := range []string{
		CodexSubscription, HermesSubscription, XAISubscription,
		MiniMaxSubscription, ClineSubscription,
	} {
		models := SubscriptionModels(subscription)
		if len(models) == 0 {
			t.Errorf("%s has no models; there is nothing to choose from", subscription)
			continue
		}
		// The default has to be in the list, or activating picks something the
		// picker cannot show as selected.
		if !IsSubscriptionModel(subscription, DefaultModelFor(subscription)) {
			t.Errorf("%s default %q is not in its own model list %v",
				subscription, DefaultModelFor(subscription), models)
		}
	}
}

// A picker that offered a model the endpoint rejects would be worse than none,
// so the membership check is the guard.
func TestIsSubscriptionModelIsExact(t *testing.T) {
	if !IsSubscriptionModel(XAISubscription, "grok-4.7") {
		t.Error("a known xAI model was rejected")
	}
	if !IsSubscriptionModel(XAISubscription, "  GROK-4.7  ") {
		t.Error("surrounding whitespace made a known model unrecognisable")
	}
	// A model from a different provider must not pass, or a crafted callback could
	// write a profile the endpoint rejects.
	if IsSubscriptionModel(XAISubscription, "MiniMax-M3") {
		t.Error("a model from another provider was accepted")
	}
	if IsSubscriptionModel("not-a-provider", "grok-4.7") {
		t.Error("an unknown provider accepted a model")
	}
}

func TestUnknownProviderHasNoModels(t *testing.T) {
	if models := SubscriptionModels("nonsense"); len(models) != 0 {
		t.Errorf("an unknown provider returned %v", models)
	}
}

// The returned slice must be a copy. A caller that sorted or appended to it would
// otherwise reorder the package-level list every other reader sees.
func TestSubscriptionModelsAreCopies(t *testing.T) {
	first := SubscriptionModels(ClineSubscription)
	if len(first) == 0 {
		t.Fatal("no cline models")
	}
	first[0] = "mutated"
	second := SubscriptionModels(ClineSubscription)
	if second[0] == "mutated" {
		t.Error("the package-level model list was mutated through the returned slice")
	}
}
