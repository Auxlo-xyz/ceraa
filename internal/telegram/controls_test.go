package telegram

import (
	"strings"
	"testing"

	"sooraa/internal/telegram/keyboard"
	"sooraa/internal/telegram/menus"
)

func TestPersistentKeyboardUsesMenuButton(t *testing.T) {
	markup := keyboard.Main()
	keyboardRows, ok := markup["keyboard"].([][]map[string]string)
	if !ok {
		t.Fatal("expected reply keyboard")
	}
	foundMenu := false
	for _, row := range keyboardRows {
		for _, button := range row {
			if button["text"] == "📋 Menu" {
				foundMenu = true
			}
			if button["text"] == "🧠 Model" {
				t.Fatal("model should be replaced by menu")
			}
		}
	}
	if !foundMenu {
		t.Fatal("menu button not found")
	}
}

func TestNormalizeCommandTextSupportsGroupBotMentions(t *testing.T) {
	if got := normalizeCommandText("/menu@SooraaBot"); got != "/menu" {
		t.Fatalf("normalizeCommandText() = %q", got)
	}
	if got := normalizeCommandText("/schedule@SooraaBot add 30m task"); got != "/schedule add 30m task" {
		t.Fatalf("normalizeCommandText() with args = %q", got)
	}
}

func TestCommandCatalogCoversBridgeControls(t *testing.T) {
	wanted := map[string]bool{
		"status": false, "settings": false, "model": false, "tools": false,
		"todos": false, "context": false, "search": false, "index": false,
		"snapshots": false, "revert": false, "permission": false,
	}
	for _, command := range commandCatalog() {
		if _, ok := wanted[command.Command]; ok {
			wanted[command.Command] = true
		}
	}
	for command, found := range wanted {
		if !found {
			t.Errorf("missing command %q", command)
		}
	}
}

func TestTelegramCommandNamesAreValid(t *testing.T) {
	for _, command := range commandNames() {
		if strings.ContainsAny(command["command"], "-.") {
			t.Fatalf("invalid Telegram command name %q", command["command"])
		}
	}
}

func TestNewFeatureMenusUseInlineButtons(t *testing.T) {
	for _, name := range []string{"memory", "skill", "eval", "schedule"} {
		markup := menus.Inline(name, 0)
		keyboard, ok := markup["inline_keyboard"].([][]map[string]string)
		if !ok || len(keyboard) == 0 {
			t.Fatalf("expected inline keyboard for %s", name)
		}
	}
}

func TestDynamicToolMenuUsesRegistry(t *testing.T) {
	markup := menus.Inline("tools", 0)
	keyboard, ok := markup["inline_keyboard"].([][]map[string]string)
	if !ok || len(keyboard) == 0 {
		t.Fatal("expected inline keyboard")
	}
	found := false
	for _, row := range keyboard {
		for _, button := range row {
			if button["callback_data"] == "tool:bash" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("expected bash tool button")
	}
}
