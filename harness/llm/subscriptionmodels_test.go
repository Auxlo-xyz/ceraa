package llm

import "testing"

// Every subscription provider needs a list, or there is nothing to pick from and
// the provider activates with a hardcoded default nothing can change.
func TestEverySubscriptionProviderHasModels(t *testing.T) {
	for _, subscription := range []string{
		CodexSubscription, HermesSubscription, XAISubscription,
		MiniMaxSubscription, ClineSubscription, DeepSeekSubscription,
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

// The list is the operator's, and its shape is worth pinning: a stale allow-list
// is silent because guardModel substitutes a default for anything missing, so a
// valid current model is quietly replaced by an older one. Nothing errors.
//
// This asserts the families are present and that every entry is reachable through
// the live gate, which is the path activation actually takes.
func TestEveryListedCodexModelPassesTheActivationGate(t *testing.T) {
	for _, model := range CodexModels {
		if !IsSubscriptionModel(CodexSubscription, model) {
			t.Errorf("%q is in CodexModels but the activation gate rejects it", model)
		}
		if !IsCodexModel(model) {
			t.Errorf("%q is in CodexModels but IsCodexModel rejects it", model)
		}
	}
}

// The gpt-6 family was missing entirely while the comment claimed the list had
// been verified against the live endpoint. That combination is what let it drift.
func TestTheCodexListCoversEveryFamily(t *testing.T) {
	families := []string{"gpt-5.6", "gpt-6", "gpt-6.1"}
	for _, family := range families {
		found := false
		for _, model := range CodexModels {
			if len(model) >= len(family) && model[:len(family)] == family {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no Codex model in the %q family", family)
		}
	}
	if len(CodexModels) < 8 {
		t.Errorf("CodexModels has %d entries, want the 8 the operator listed", len(CodexModels))
	}
}

// The default has to be a member of the list, or activation substitutes a model
// that is then not itself in the list.
func TestTheDefaultCodexModelIsOnTheList(t *testing.T) {
	if !IsCodexModel(DefaultCodexModel) {
		t.Errorf("DefaultCodexModel = %q, which is not in CodexModels", DefaultCodexModel)
	}
}

// The picker and the catalog have to agree. OpenCodeFreeModels is the upstream
// free set the picker shows first, so every id in it must be in the catalog
// and first in the same order, and the set must hold no duplicates -
// otherwise the user is sent to a model the picker will not offer.
func TestTheFreeModelsAreInTheCatalogAndFirst(t *testing.T) {
	if len(OpenCodeFreeModels) == 0 {
		t.Fatal("OpenCodeFreeModels is empty; the hint has nothing to point at")
	}
	if OpenCodeFreeModels[0] != DefaultOpenCodeModel {
		t.Errorf("OpenCodeFreeModels[0] = %q, want the default %q", OpenCodeFreeModels[0], DefaultOpenCodeModel)
	}
	seen := map[string]int{}
	for i, id := range OpenCodeFreeModels {
		if !IsOpenCodeModel(id) {
			t.Errorf("OpenCodeFreeModels[%d] = %q is not in OpenCodeModels", i, id)
		}
		seen[id]++
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("%q appears %d times in OpenCodeFreeModels", id, count)
		}
	}
	// The free ids are the head of the catalog, in the same order, so the
	// top of the picker is exactly the upstream free set.
	for i, id := range OpenCodeFreeModels {
		if OpenCodeModels[i] != id {
			t.Fatalf("OpenCodeModels[%d] = %q, want %q; the free ids are meant to lead the catalog",
				i, OpenCodeModels[i], id)
		}
	}
}

// A model that cannot answer from here must not be presented as one that can.
// jev-1.13 and ling-3.0-flash-fin answer ModelProtocolUnsupported and 404
// respectively; either of them sitting in the free answer set would make the
// hint send a user into a refusal.
func TestTheFreeAnswerSetDropsTheOnesThatRefuse(t *testing.T) {
	refused := []string{"jev-1.13-free", "ling-3.0-flash-fin-free", "deepseek-v4-flash-free"}
	for _, id := range refused {
		for _, free := range OpenCodeFreeModels {
			if id == free {
				t.Errorf("%q does not answer from here but is listed as a free answer", id)
			}
		}
	}
}

// A retired model id must not be offered anywhere. Upstream answers 410
// ModelDeprecated for exo-free and fledge-alpha-free, so either of them in
// the free set or the catalog would make the picker send a user into a
// refusal for an id that no longer exists.
func TestTheCatalogDropsDeprecatedIds(t *testing.T) {
	for _, id := range []string{"exo-free", "fledge-alpha-free"} {
		for _, free := range OpenCodeFreeModels {
			if id == free {
				t.Errorf("%q is retired upstream but is listed as a free answer", id)
			}
		}
		if IsOpenCodeModel(id) {
			t.Errorf("%q is retired upstream but is still in the catalog", id)
		}
	}
}

// DeepSeek rides the subscription path end to end: the predicate that decides
// whether a profile is configured, the Messages transport, and the model
// picker all key off these three values, so they are asserted together. A
// mismatch between any two is a provider that looks configured in one place
// and unconfigured in another.
func TestDeepSeekSubscriptionWiringAgrees(t *testing.T) {
	if !IsSubscription(DeepSeekSubscription) {
		t.Error("deepseek is not counted as a subscription; doctor and the startup banner will report it unconfigured")
	}
	if DeepSeekBaseURL == "" || DeepSeekMessagesAuth == "" {
		t.Error("the DeepSeek endpoint or header style is empty")
	}
	if DeepSeekMessagesAuth != MessagesAuthDsh {
		t.Errorf("DeepSeekMessagesAuth = %q, want the dsh wire style the transport sends as x-dsh-auth-token", DeepSeekMessagesAuth)
	}
	if DeepSeekContextWindow(DefaultDeepSeekModel) <= 0 {
		t.Errorf("the default model %q has no known context window", DefaultDeepSeekModel)
	}
	if DeepSeekContextWindow("not-a-model") != 0 {
		t.Error("an unknown model got a window instead of zero")
	}
}
