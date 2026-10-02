package telegram

import (
	"fmt"

	"ceraa/internal/miniapp"
)

// webSettings is what a run on the Mini App surface will use.
//
// No key and no token appears here. The provider id, the model it points at and
// the agent mode are the things a person recognises from the menu; the
// credential behind them stays where it is, because this response is one the
// browser keeps in memory for as long as the tab is open.
func webSettings() map[string]any {
	model, agent := defaultState.values()
	name, profile, _ := defaultState.activeProviderProfile()
	auto, _ := defaultState.autoCompaction()
	return map[string]any{
		"provider":        defaultState.provider(),
		"provider_name":   name,
		"provider_model":  profile.Model,
		"model":           model,
		"agent":           agent,
		"reasoning":       defaultState.reasoning(),
		"auto_compaction": auto,
		"workspace":       configuredWorkspaceRoot(),
		"session":         miniapp.WebSessionID,
	}
}

// webSettingKeys is the vocabulary a body may use. Anything else is refused
// before a single one of them is applied, so a typo cannot half-save a form.
var webSettingKeys = map[string]string{
	"reasoning":       "string",
	"provider":        "string",
	"agent":           "string",
	"model":           "string",
	"auto_compaction": "bool",
}

// applyWebSettings changes what the next run uses.
//
// The checks that can reject a value run first and in an order where nothing has
// been written yet: reasoning validates against a known list and the provider
// has to exist, and both are asked before the permissive ones run. If a domain
// check still fails partway, the settings that were applied are the ones that
// were valid, and the error says which one stopped it.
func applyWebSettings(update map[string]any) error {
	for key, raw := range update {
		want, known := webSettingKeys[key]
		if !known {
			return fmt.Errorf("unknown setting %q", key)
		}
		switch want {
		case "string":
			if _, ok := raw.(string); !ok {
				return fmt.Errorf("setting %q must be a string", key)
			}
		case "bool":
			if _, ok := raw.(bool); !ok {
				return fmt.Errorf("setting %q must be true or false", key)
			}
		}
	}

	order := []string{"reasoning", "provider", "agent", "model", "auto_compaction"}
	for _, key := range order {
		raw, ok := update[key]
		if !ok {
			continue
		}
		switch key {
		case "reasoning":
			if err := defaultState.setReasoning(raw.(string)); err != nil {
				return err
			}
		case "provider":
			if err := defaultState.setProvider(raw.(string)); err != nil {
				return err
			}
		case "agent":
			if err := defaultState.setAgent(raw.(string)); err != nil {
				return err
			}
		case "model":
			if err := defaultState.setModel(raw.(string)); err != nil {
				return err
			}
		case "auto_compaction":
			if err := defaultState.setAutoCompaction(raw.(bool)); err != nil {
				return err
			}
		}
	}
	return nil
}
