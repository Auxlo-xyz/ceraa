package menus

import "testing"

func TestRootMenuHasFiveRowsAndPrimaryControls(t *testing.T) {
	markup := Root()
	rows, ok := markup["inline_keyboard"].([][]map[string]string)
	if !ok || len(rows) != 5 {
		t.Fatalf("expected five-line root keyboard, got %T with %d rows", markup["inline_keyboard"], len(rows))
	}
	found := map[string]bool{}
	for _, row := range rows {
		for _, button := range row {
			found[button["callback_data"]] = true
		}
	}
	for _, callback := range []string{"menu:status", "menu:session", "menu:model", "menu:agent", "menu:variant", "menu:thinking", "menu:project", "menu:tools", "menu:todos", "close"} {
		if !found[callback] {
			t.Errorf("missing root callback %q", callback)
		}
	}
}

// TestThinkingMenuOffersLevelsAndDefault pins that a reasoning level can be
// chosen and that the provider default remains reachable.
func TestThinkingMenuOffersLevelsAndDefault(t *testing.T) {
	markup := thinkingMenu()
	rows, ok := markup["inline_keyboard"].([][]map[string]string)
	if !ok {
		t.Fatalf("thinking menu = %#v", markup)
	}
	found := map[string]bool{}
	for _, row := range rows {
		for _, button := range row {
			found[button["callback_data"]] = true
		}
	}
	for _, level := range []string{"off", "minimal", "low", "medium", "high", "max", "default"} {
		if !found["select_reasoning:"+level] {
			t.Errorf("missing thinking level %q", level)
		}
	}
}

func TestTitleSettingsMenuUsesInlineControls(t *testing.T) {
	markup := titleSettingsMenu()
	rows, ok := markup["inline_keyboard"].([][]map[string]string)
	if !ok {
		t.Fatal("expected inline keyboard")
	}
	want := map[string]bool{
		"title_model:default": false,
		"title_model:custom":  false,
		"title_key:set":       false,
		"title_test":          false,
	}
	for _, row := range rows {
		for _, button := range row {
			if _, exists := want[button["callback_data"]]; exists {
				want[button["callback_data"]] = true
			}
		}
	}
	for callback, found := range want {
		if !found {
			t.Errorf("missing callback %s", callback)
		}
	}
}
