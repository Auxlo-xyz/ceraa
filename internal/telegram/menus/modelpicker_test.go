package menus

import (
	"encoding/json"
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
