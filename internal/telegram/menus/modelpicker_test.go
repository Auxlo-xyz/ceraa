package menus

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"ceraa/harness/llm"
)

// There was no way to change a subscription provider's model: the remote catalog
// is a shared list keyed on OpenAI-compatible hosts and never carried these
// models, so a provider activated with a hardcoded default and nothing could
// move it. The picker is the fix, and this asserts it exists and is reachable.
func TestSubscriptionProvidersOfferAModelPicker(t *testing.T) {
	for _, providerID := range []string{"codex", "hermes", "xai", "minimax", "cline"} {
		raw, _ := json.Marshal(InlineForChat(providerID+":models", 0, 1, ""))
		if !strings.Contains(string(raw), "provider:"+providerID+":model:") {
			t.Errorf("/%s:models has no model buttons: %s", providerID, raw)
		}
		if !strings.Contains(string(raw), "menu:"+providerID) {
			t.Errorf("/%s:models offers no way back: %s", providerID, raw)
		}
	}
}

// A signed-in provider's menu must reach the picker, or the picker is unreachable
// from anywhere a user would look.
func TestSignedInSubscriptionMenuOffersModels(t *testing.T) {
	for _, providerID := range []string{"codex", "hermes", "xai", "minimax", "cline"} {
		raw, _ := json.Marshal(subscriptionMenu("Test", "🔑", true, true, "provider:"+providerID))
		if !strings.Contains(string(raw), providerID+":models") {
			t.Errorf("an active %s menu has no model row: %s", providerID, raw)
		}
	}
}

// Every model offered has to be one the provider actually serves.
func TestPickerOffersOnlyKnownModels(t *testing.T) {
	raw, _ := json.Marshal(InlineForChat("cline:models", 0, 1, ""))
	seen := 0
	for _, row := range InlineForChat("cline:models", 0, 1, "")["inline_keyboard"].([][]map[string]string) {
		for _, button := range row {
			data := button["callback_data"]
			if !strings.HasPrefix(data, "provider:cline:model:") {
				continue
			}
			seen++
			model := strings.TrimPrefix(data, "provider:cline:model:")
			if !isKnownClineModel(model) {
				t.Errorf("the picker offers %q, which the provider does not serve", model)
			}
		}
	}
	if seen == 0 {
		t.Errorf("the picker has no models: %s", raw)
	}
}

func isKnownClineModel(model string) bool {
	return llm.IsSubscriptionModel(llm.ClineSubscription, model)
}

// /model went to the remote catalog regardless of provider, so a signed-in
// account saw a list of namespaced ids for hosts its gateway does not serve.
// Asking for one of those produces a request the endpoint cannot answer.
func TestCatalogShowsTheActiveSubscriptionProvidersOwnModels(t *testing.T) {
	for _, providerID := range []string{"codex", "hermes", "xai", "minimax", "cline"} {
		t.Run(providerID, func(t *testing.T) {
			withActiveProvider(t, providerID)
			raw, _ := json.Marshal(ModelCatalogForChat(1, ""))
			body := string(raw)

			// Labelled provider/model, so it reads as belonging to the account.
			label := subscriptionLabel(providerID)
			if !strings.Contains(body, label+"/") {
				t.Errorf("the catalog is not qualified with %s: %s", label, body)
			}
			// A namespaced id from the remote catalog must not appear, because the
			// gateway would not serve it.
			if strings.Contains(body, "aion-labs/") || strings.Contains(body, "amazon/nova") {
				t.Errorf("the remote catalog leaked into %s's model list: %s", providerID, body)
			}
		})
	}
}

// The stored id must be exactly what the gateway expects. Only the label is
// qualified, because a model id the provider does not serve is a hard failure and
// a display name is never sent.
func TestCatalogKeepsTheExactGatewayModelID(t *testing.T) {
	restore := withActiveProvider(t, "cline")
	defer restore()

	ModelCatalogForChat(1, "")
	entries := subscriptionCatalogEntries("cline")
	if len(entries) == 0 {
		t.Fatal("no catalog entries")
	}
	known := llm.SubscriptionModels(llm.ClineSubscription)
	for _, entry := range entries {
		if !llm.IsSubscriptionModel(llm.ClineSubscription, entry.ID) {
			t.Errorf("catalog id %q is not a model the gateway serves", entry.ID)
		}
		// And the label must carry the provider name.
		if !strings.HasPrefix(entry.Name, "Cline/") && !strings.HasPrefix(entry.Name, "✅ Cline/") {
			t.Errorf("label %q is not qualified with the provider", entry.Name)
		}
	}
	_ = known
}

// Every offered entry has to be selectable, or the button is decorative.
func TestCatalogEntriesAreSelectable(t *testing.T) {
	restore := withActiveProvider(t, "xai")
	defer restore()

	keyboard := ModelCatalogForChat(1, "")
	rows := keyboard["inline_keyboard"].([][]map[string]string)
	selections := 0
	for _, row := range rows {
		for _, button := range row {
			if strings.HasPrefix(button["callback_data"], "model:") {
				selections++
			}
		}
	}
	if selections != len(llm.SubscriptionModels(llm.XAISubscription)) {
		t.Errorf("%d selectable entries for %d models", selections, len(llm.SubscriptionModels(llm.XAISubscription)))
	}
}

// The buttons carry an index, and the callback resolves it through modelChoices.
// If that is not populated for this provider, the index resolves against whatever
// was left from a previous catalog and silently picks the wrong model.
func TestCatalogChoiceResolvesToTheOfferedModel(t *testing.T) {
	restore := withActiveProvider(t, "cline")
	defer restore()

	keyboard := ModelCatalogForChat(1, "")
	rows := keyboard["inline_keyboard"].([][]map[string]string)
	checked := 0
	for _, row := range rows {
		for _, button := range row {
			data := button["callback_data"]
			if !strings.HasPrefix(data, "model:") {
				continue
			}
			index, err := strconv.Atoi(strings.TrimPrefix(data, "model:"))
			if err != nil {
				t.Fatalf("button %q has no usable index", data)
			}
			choice, ok := ModelChoice(index)
			if !ok {
				t.Fatalf("index %d from %q resolves to nothing; the choice is a no-op", index, data)
			}
			// The button's text is the label, so the resolved id has to be one of
			// the provider's own models, not a leftover from the remote catalog.
			if !llm.IsSubscriptionModel(llm.ClineSubscription, choice.ID) {
				t.Errorf("index %d resolved to %q, which the gateway does not serve", index, choice.ID)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Error("no model buttons to check")
	}
}

// A non-subscription provider must keep the remote catalog, or the change breaks
// every ordinary custom provider.
func TestNonSubscriptionProviderKeepsTheRemoteCatalog(t *testing.T) {
	restore := withActiveProvider(t, "custom")
	defer restore()
	// An unknown provider falls through, which is the same path.
	if entries := subscriptionCatalogEntries("custom"); entries != nil {
		t.Errorf("a plain provider got a subscription catalog: %v", entries)
	}
}
