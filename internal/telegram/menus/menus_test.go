package menus

import "testing"

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
