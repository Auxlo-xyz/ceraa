package telegram

import (
	"encoding/json"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/telegram/menus"
)

// Every button a provider screen renders must resolve to a route. An unhandled
// callback has no fallback: the tap answers with a spinner and nothing else.
//
// This is a real bug, not a hypothetical one. The model button emitted
// "provider:<id>:models" while the handler matched "provider:<id>:model:<model>"
// and nothing else, so Model on a provider's screen did nothing, and the previous
// provider's model list stayed on screen looking current.
func TestProviderScreenButtonsAreAllRouted(t *testing.T) {
	// switchableProviderIDs, not subscriptionProviderIDs: subscriptionMenu is only
	// what the four switchable providers actually render. Codex has its own screen.
	for _, providerID := range switchableProviderIDs {
		menu := menus.SubscriptionMenuForTest(menus.SubscriptionLabel(providerID), providerID, true, false)
		for _, row := range menu["inline_keyboard"].([][]map[string]string) {
			for _, btn := range row {
				if !callbackIsRouted(btn["callback_data"]) {
					t.Errorf("%s screen button %q emits %q, which resolves to no route",
						providerID, btn["text"], btn["callback_data"])
				}
			}
		}
	}
}

// The model list is scoped to the provider whose button opened it. Reading the
// model list of a provider you are not currently on is the normal way to decide
// before switching, so the two come apart constantly.
func TestSubscriptionModelMenuIsScopedToItsProvider(t *testing.T) {
	xai := strings.Join(flattenButtons(menus.SubscriptionModelMenu("xai")), "\n")
	minimax := strings.Join(flattenButtons(menus.SubscriptionModelMenu("minimax")), "\n")

	if strings.Contains(xai, "MiniMax") {
		t.Errorf("the xAI model list contains a MiniMax model:\n%s", xai)
	}
	if strings.Contains(minimax, "grok") {
		t.Errorf("the MiniMax model list contains an xAI model:\n%s", minimax)
	}
	if !strings.Contains(xai, "provider:xai:model:") {
		t.Errorf("xAI model buttons do not route back through xAI:\n%s", xai)
	}
	if !strings.Contains(minimax, "provider:minimax:model:") {
		t.Errorf("MiniMax model buttons do not route back through MiniMax:\n%s", minimax)
	}
}

func TestRouteProviderCallbackShapes(t *testing.T) {
	cases := []struct {
		data     string
		provider string
		kind     providerRouteKind
		value    string
	}{
		{"provider:xai", "xai", providerRouteActivate, ""},
		{"provider:xai:models", "xai", providerRouteModels, ""},
		{"provider:xai:model:grok-4.7", "xai", providerRoutePickModel, "grok-4.7"},
		{"provider:xai:relogin", "xai", providerRouteRelogin, ""},
		// Not subscription providers, or not this family at all.
		{"provider:custom", "", providerRouteNone, ""},
		{"provider:remove_menu", "", providerRouteNone, ""},
		{"provider:model_manual", "", providerRouteNone, ""},
		// Codex has a model list but its own activate and login callbacks, so
		// "provider:codex" and "provider:codex:relogin" are deliberately not routes.
		{"provider:codex:models", "codex", providerRouteModels, ""},
		{"provider:codex:model:" + llm.DefaultModelFor(llm.CodexSubscription), "codex", providerRoutePickModel, llm.DefaultModelFor(llm.CodexSubscription)},
		{"provider:codex", "", providerRouteNone, ""},
		{"provider:codex:relogin", "", providerRouteNone, ""},
		{"menu:xai", "", providerRouteNone, ""},
		{"provider:xai:model:", "", providerRouteNone, ""},
		{"provider:xai:unknown", "", providerRouteNone, ""},
	}
	for _, tc := range cases {
		provider, kind, value := routeProviderCallback(tc.data)
		if provider != tc.provider || kind != tc.kind || value != tc.value {
			t.Errorf("routeProviderCallback(%q) = (%q, %v, %q), want (%q, %v, %q)",
				tc.data, provider, kind, value, tc.provider, tc.kind, tc.value)
		}
	}
}

func callbackIsRouted(data string) bool {
	if data == "noop" || data == "close" || data == "new_session" {
		return true
	}
	for _, exact := range []string{
		"provider:remove_menu", "provider:model_manual", "provider:custom",
		"codex:login", "codex:activate", "model_catalog_search",
	} {
		if data == exact {
			return true
		}
	}
	for _, prefix := range []string{
		"menu:", "model:", "select_provider:", "session_", "project:",
		"browse:", "show_messages:", "tool:", "todo:", "permission:",
		"perm:", "qtype:", "select_agent:", "select_reasoning:",
	} {
		if strings.HasPrefix(data, prefix) {
			return true
		}
	}
	_, kind, _ := routeProviderCallback(data)
	return kind != providerRouteNone
}

func flattenButtons(menu map[string]any) []string {
	var out []string
	for _, row := range menu["inline_keyboard"].([][]map[string]string) {
		for _, btn := range row {
			out = append(out, btn["text"]+" -> "+btn["callback_data"])
		}
	}
	return out
}

// The real Codex screen, not the one subscriptionMenu would build. Codex has its
// own callbacks, and asserting against a menu it never renders would pass without
// saying anything about the screen a user actually taps.
func TestCodexScreenButtonsAreRouted(t *testing.T) {
	raw, err := json.Marshal(menus.CodexMenuForTest(true))
	if err != nil {
		t.Fatalf("marshal codex menu: %v", err)
	}
	rows, ok := menus.CodexMenuForTest(true)["inline_keyboard"].([][]map[string]string)
	if !ok {
		t.Fatalf("codex menu has no rows: %s", raw)
	}
	for _, row := range rows {
		for _, btn := range row {
			if !callbackIsRouted(btn["callback_data"]) {
				t.Errorf("codex screen button %q emits %q, which resolves to no route",
					btn["text"], btn["callback_data"])
			}
		}
	}
}
