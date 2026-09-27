package telegram

import (
	"encoding/json"
	"strings"
	"testing"

	"ceraa/internal/config"
	"ceraa/internal/telegram/menus"
)

func callbacksIn(t *testing.T, markup map[string]any) []string {
	t.Helper()
	rows, ok := markup["inline_keyboard"].([][]map[string]string)
	if !ok {
		t.Fatalf("markup is not an inline keyboard: %T", markup["inline_keyboard"])
	}
	var out []string
	for _, row := range rows {
		for _, button := range row {
			out = append(out, button["callback_data"])
		}
	}
	return out
}

func labelsIn(t *testing.T, markup map[string]any) string {
	t.Helper()
	rows := markup["inline_keyboard"].([][]map[string]string)
	raw, _ := json.Marshal(rows)
	return string(raw)
}

func hasCallback(callbacks []string, want string) bool {
	for _, callback := range callbacks {
		if callback == want {
			return true
		}
	}
	return false
}

func seedProfile(t *testing.T, state *bridgeState, id string) {
	t.Helper()
	if err := state.setProviderProfile(id, config.ProviderProfile{
		URL: "https://" + id + ".example/v1", Model: id + "/model",
	}); err != nil {
		t.Fatalf("seed profile %s: %v", id, err)
	}
}

// The defect: the select menu merged config.json with bridge-state.json, the
// remove menu listed only bridge-state.json, and removal deleted only from
// bridge-state.json. A provider held in both was removed from one store and
// reappeared immediately, which is what made removal look broken.
func TestRemovingAProviderRemovesItFromEveryStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)

	// The same id in both stores, as happens when a provider is hand-added to
	// config.json and also created from Telegram.
	cfg := config.DefaultConfig()
	cfg.ProviderProfiles = map[string]config.ProviderProfile{
		"both": {URL: "https://both.example/v1", Model: "both/model"},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	previous := defaultState
	state := loadBridgeStateFromFile(dir + "/bridge-state.json")
	seedProfile(t, state, "both")
	defaultState = state
	t.Cleanup(func() { defaultState = previous })

	// It is listed once, and offered for removal.
	if !hasCallback(callbacksIn(t, menus.ProviderMenu()), "select_provider:both") {
		t.Fatal("the provider is not in the provider menu, so the test would prove nothing")
	}
	if !hasCallback(callbacksIn(t, menus.RemoveProviderMenu(state.provider())), "provider:remove:both") {
		t.Fatal("the provider is not offered for removal")
	}

	// Through the single production entry point, which is what the confirm
	// callback calls. Driving the helpers separately would pass even if the
	// callback only cleared one store, which is the original bug.
	fromState, fromConfig, stateErr, configErr := removeProviderEverywhere("both")
	if stateErr != nil || configErr != nil {
		t.Fatalf("removal reported errors: state=%v config=%v", stateErr, configErr)
	}
	if !fromState || !fromConfig {
		t.Fatalf("removal cleared state=%v config=%v, want both", fromState, fromConfig)
	}

	// The point of the fix: it is gone from the menu that showed it.
	if hasCallback(callbacksIn(t, menus.ProviderMenu()), "select_provider:both") {
		t.Error("the provider is still listed after being removed from both stores")
	}
	if hasCallback(callbacksIn(t, menus.RemoveProviderMenu(state.provider())), "provider:remove:both") {
		t.Error("the provider is still offered for removal after being removed")
	}
}

// The other half of the same defect: a provider that lives only in config.json was
// selectable but had no row in the remove menu, so there was nothing to tap.
func TestConfigOnlyProviderIsOfferedForRemoval(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	cfg := config.DefaultConfig()
	cfg.ProviderProfiles = map[string]config.ProviderProfile{
		"cfgonly": {URL: "https://cfg.example/v1", Model: "cfg/model"},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	previous := defaultState
	defaultState = loadBridgeStateFromFile(dir + "/bridge-state.json")
	t.Cleanup(func() { defaultState = previous })

	if !hasCallback(callbacksIn(t, menus.ProviderMenu()), "select_provider:cfgonly") {
		t.Fatal("a config-only provider is not listed, so the test would prove nothing")
	}
	if !hasCallback(callbacksIn(t, menus.RemoveProviderMenu("")), "provider:remove:cfgonly") {
		t.Error("a config-only provider is selectable but cannot be removed")
	}
}

// Removing the active provider has to move the bot off it, from whichever store
// held it.
func TestRemovingTheActiveProviderClearsTheSelection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	previous := defaultState
	state := loadBridgeStateFromFile(dir + "/bridge-state.json")
	seedProfile(t, state, "live")
	if err := state.setProvider("live"); err != nil {
		t.Fatal(err)
	}
	defaultState = state
	t.Cleanup(func() { defaultState = previous })

	if state.provider() != "live" {
		t.Fatalf("precondition: active provider is %q", state.provider())
	}
	if _, err := state.removeProviderProfile("live"); err != nil {
		t.Fatal(err)
	}
	if state.provider() == "live" {
		t.Error("the bot still points at a provider that no longer exists")
	}
}

// The base configuration is not a stored profile and must stay unremovable,
// whichever menu offers it.
func TestDefaultIsNeverOfferedForRemoval(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	previous := defaultState
	defaultState = loadBridgeStateFromFile(dir + "/bridge-state.json")
	t.Cleanup(func() { defaultState = previous })
	if hasCallback(callbacksIn(t, menus.RemoveProviderMenu("")), "provider:remove:default") {
		t.Error("the remove menu offers the default provider")
	}
}

// The split: provider identity and model choice are separate screens.
func TestProviderAndModelAreSeparateMenus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	previous := defaultState
	state := loadBridgeStateFromFile(dir + "/bridge-state.json")
	seedProfile(t, state, "alpha")
	if err := state.setProvider("alpha"); err != nil {
		t.Fatal(err)
	}
	defaultState = state
	t.Cleanup(func() { defaultState = previous })

	provider := callbacksIn(t, menus.ProviderMenu())
	// The provider menu lists providers and nothing about choosing a model.
	if !hasCallback(provider, "select_provider:alpha") {
		t.Error("the provider menu does not list providers")
	}
	if hasCallback(provider, "menu:model_catalog") {
		t.Error("the provider menu still offers model selection, so the split is not done")
	}

	model := callbacksIn(t, menus.ModelMenuForTest())
	if !hasCallback(model, "menu:model_catalog") {
		t.Error("the model menu does not offer model selection")
	}
	// And it names the model actually answering, with its details.
	labels := labelsIn(t, menus.ModelMenuForTest())
	for _, want := range []string{"alpha/model", "Provider: alpha", "Endpoint: alpha.example"} {
		if !strings.Contains(labels, want) {
			t.Errorf("the model menu does not show %q: %s", want, labels)
		}
	}
}

// With no model chosen the model menu has to say so rather than showing an empty
// screen or a stale name.
func TestModelMenuWithNoActiveModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	previous := defaultState
	defaultState = loadBridgeStateFromFile(dir + "/bridge-state.json")
	t.Cleanup(func() { defaultState = previous })
	labels := labelsIn(t, menus.ModelMenuForTest())
	if !strings.Contains(labels, "No model is active") {
		t.Errorf("model menu with no model says %q", labels)
	}
}
