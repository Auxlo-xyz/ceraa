package menus

import "testing"

func TestRootMenuHasFourRowsAndPrimaryControls(t *testing.T) {
	markup := Root()
	rows, ok := markup["inline_keyboard"].([][]map[string]string)
	if !ok || len(rows) != 4 {
		t.Fatalf("expected four-line root keyboard, got %T with %d rows", markup["inline_keyboard"], len(rows))
	}
	found := map[string]bool{}
	for _, row := range rows {
		for _, button := range row {
			found[button["callback_data"]] = true
		}
	}
	for _, callback := range []string{"menu:status", "menu:session", "menu:model", "menu:agent", "menu:variant", "menu:project", "menu:tools", "menu:todos", "close"} {
		if !found[callback] {
			t.Errorf("missing root callback %q", callback)
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
