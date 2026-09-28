package menus

import (
	"encoding/json"
	"testing"
)

func TestRootMenuHasPrimaryControls(t *testing.T) {
	markup := Root()
	rows, ok := markup["inline_keyboard"].([][]map[string]string)
	// Six lines since provider and model became separate entries, which is the
	// point of the split: the compact menu has to offer both.
	if !ok || len(rows) != 6 {
		t.Fatalf("expected six-line root keyboard, got %T with %d rows", markup["inline_keyboard"], len(rows))
	}
	found := map[string]bool{}
	for _, row := range rows {
		for _, button := range row {
			found[button["callback_data"]] = true
		}
	}
	for _, callback := range []string{"menu:status", "menu:session", "menu:provider", "menu:model", "menu:agent", "menu:thinking", "menu:project", "menu:tools", "menu:todos", "close"} {
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

// The hub is a person's menu, and the tool registry is the agent's. Two
// identical buttons both labelled "Tools" and both opening toolsMenu() read as a
// mistake rather than as a feature.
func TestTheHubDoesNotShowTheToolRegistry(t *testing.T) {
	raw, _ := json.Marshal(Hub())
	rows := Hub()["inline_keyboard"].([][]map[string]string)
	for _, row := range rows {
		for _, button := range row {
			switch button["callback_data"] {
			case "menu:tools", "menu:skills_catalog":
				t.Errorf("the hub offers the tool registry: %s in %s", button["text"], raw)
			}
		}
	}
	// Every other entry is still there, so hiding two did not empty the hub.
	shown := 0
	for _, row := range rows {
		shown += len(row)
	}
	if shown < len(All)-2 {
		t.Errorf("the hub shows %d buttons for %d entries; hiding two removed more", shown, len(All))
	}
}

// Hidden from the hub is not removed: a keyboard sent before this change still
// resolves, rather than arriving as a dead button.
func TestTheHiddenToolMenusStillDispatch(t *testing.T) {
	for _, name := range []string{"tools", "skills_catalog"} {
		keyboard := InlineForChat(name, 0, 1, "")
		rows, ok := keyboard["inline_keyboard"].([][]map[string]string)
		if !ok || len(rows) == 0 {
			t.Fatalf("%s no longer resolves to a menu", name)
		}
		dead := true
		for _, row := range rows {
			for _, button := range row {
				if button["callback_data"] != "close" {
					dead = false
				}
			}
		}
		if dead {
			t.Errorf("%s resolves to a bare Close button", name)
		}
	}
}
