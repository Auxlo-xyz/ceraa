package telegram

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ceraa/internal/config"
	"ceraa/internal/telegram/keyboard"
	"ceraa/internal/telegram/menus"
)

func TestPersistentKeyboardUsesMenuButton(t *testing.T) {
	markup := keyboard.Main()
	keyboardRows, ok := markup["keyboard"].([][]map[string]string)
	if !ok {
		t.Fatal("expected reply keyboard")
	}
	if len(keyboardRows) != 2 || len(keyboardRows[0]) != 2 || len(keyboardRows[1]) != 2 {
		t.Fatalf("expected 2x2 reply keyboard, got %#v", keyboardRows)
	}
	foundHub := false
	foundMode := false
	for _, row := range keyboardRows {
		for _, button := range row {
			if button["text"] == "📋 Hub" {
				foundHub = true
			}
			if button["text"] == "🤖 Mode" {
				foundMode = true
			}
			if button["text"] == "🤖 Agent" || button["text"] == "📋 Menu" {
				t.Fatal("old keyboard labels should be replaced")
			}
		}
	}
	if !foundHub || !foundMode {
		t.Fatal("hub and mode buttons not found")
	}
}

func TestNormalizeCommandTextSupportsGroupBotMentions(t *testing.T) {
	if got := normalizeCommandText("/menu@CeraaBot"); got != "/menu" {
		t.Fatalf("normalizeCommandText() = %q", got)
	}
	if got := normalizeCommandText("/schedule@CeraaBot add 30m task"); got != "/schedule add 30m task" {
		t.Fatalf("normalizeCommandText() with args = %q", got)
	}
}

func TestCommandCatalogCoversBridgeControls(t *testing.T) {
	wanted := map[string]bool{
		"status": false, "settings": false, "model": false, "tools": false,
		"todos": false, "context": false, "search": false, "index": false,
		"snapshots": false, "revert": false, "permission": false, "hub": false,
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

func TestSetMyCommandsPayloadUsesCommandsObject(t *testing.T) {
	body, err := setMyCommandsBody()
	if err != nil {
		t.Fatalf("setMyCommandsBody() error = %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("setMyCommands payload is not an object: %v", err)
	}
	commands, ok := payload["commands"]
	if !ok {
		t.Fatal("setMyCommands payload is missing commands")
	}
	var decoded []map[string]string
	if err := json.Unmarshal(commands, &decoded); err != nil || len(decoded) == 0 {
		t.Fatal("setMyCommands commands must be a non-empty array")
	}
}

func TestSendWithKeyboardFallsBackAfterMarkdownRejection(t *testing.T) {
	var modes []any
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		modes = append(modes, body["parse_mode"])
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: can't parse entities"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":9}}`))
	}))
	defer server.Close()
	oldBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	defer func() { telegramAPIBase = oldBase }()

	if got := sendWithKeyboard("token", 42, "*broken*", nil); got != 9 {
		t.Fatalf("message ID = %d", got)
	}
	if len(modes) != 2 || modes[0] != "Markdown" || modes[1] != nil {
		t.Fatalf("parse modes = %#v", modes)
	}
}

func TestResolveAllowedUserIDFailsClosed(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		set       bool
		configID  int64
		stateID   int64
		want      int64
		wantError bool
	}{
		{name: "positive env", raw: "42", set: true, want: 42},
		{name: "state fallback", stateID: 42, want: 42},
		{name: "blank env", set: true, wantError: true},
		{name: "malformed env", raw: "not-an-id", set: true, wantError: true},
		{name: "zero env", raw: "0", set: true, wantError: true},
		{name: "negative config", configID: -1, wantError: true},
		{name: "negative state", stateID: -1, wantError: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveAllowedUserID(test.raw, test.set, test.configID, test.stateID)
			if (err != nil) != test.wantError || (!test.wantError && got != test.want) {
				t.Fatalf("resolveAllowedUserID() = %d, %v", got, err)
			}
		})
	}
}

func TestAuthorizationFailsClosedWithoutOwner(t *testing.T) {
	if authorized(0, 42, 42) {
		t.Fatal("zero allowlist must not authorize")
	}
	if !bootstrapStartAuthorized(0, 42, 42, "/start") {
		t.Fatal("private bootstrap start should be allowed")
	}
	if bootstrapClaimAuthorized(0, 42, -42, "onboarding:claim") {
		t.Fatal("group claim should not be allowed")
	}
}

func TestTokenOnlyStartupIsAccepted(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	startup, err := resolveTelegramStartup(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if startup.token != "test-token" || startup.providerReady {
		t.Fatalf("token-only startup = %#v", startup)
	}
}

func TestEmptyMenusExplainTheirState(t *testing.T) {
	markup := menus.Inline("mcp_catalog", 0)
	keyboardRows, ok := markup["inline_keyboard"].([][]map[string]string)
	if !ok || len(keyboardRows) == 0 {
		t.Fatal("expected empty-state keyboard")
	}
	found := false
	for _, row := range keyboardRows {
		for _, button := range row {
			if strings.Contains(button["text"], "No MCP servers configured") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("expected an explanation in the empty-state menu")
	}
}

func TestOnboardingMenuUsesInlineProviderChoices(t *testing.T) {
	markup := menus.Onboarding()
	keyboardRows, ok := markup["inline_keyboard"].([][]map[string]string)
	if !ok || len(keyboardRows) < 3 {
		t.Fatal("expected onboarding inline keyboard")
	}
	found := map[string]bool{}
	for _, row := range keyboardRows {
		for _, button := range row {
			found[button["callback_data"]] = true
		}
	}
	for _, callback := range []string{"onboarding:provider:openrouter", "onboarding:provider:poolside", "onboarding:provider:custom", "onboarding:claim"} {
		if !found[callback] {
			t.Fatalf("missing onboarding button %q", callback)
		}
	}
}

func TestModelMenuOffersProviderButtons(t *testing.T) {
	markup := menus.Inline("model", 0)
	keyboardRows, ok := markup["inline_keyboard"].([][]map[string]string)
	if !ok {
		t.Fatal("expected model inline keyboard")
	}
	foundProvider := false
	foundCustom := false
	for _, row := range keyboardRows {
		for _, button := range row {
			if button["callback_data"] == "select_provider:default" {
				foundProvider = true
			}
			if button["callback_data"] == "provider:custom" {
				foundCustom = true
			}
		}
	}
	if !foundProvider || !foundCustom {
		t.Fatal("expected saved-provider and custom-provider buttons")
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

func TestClearPromptQueueReleasesChat(t *testing.T) {
	promptQueue.mu.Lock()
	promptQueue.busy[42] = true
	promptQueue.queue[42] = []string{"first", "second"}
	promptQueue.mu.Unlock()

	clearPromptQueue(42)

	promptQueue.mu.Lock()
	defer promptQueue.mu.Unlock()
	if promptQueue.busy[42] {
		t.Fatal("prompt queue remained busy")
	}
	if len(promptQueue.queue[42]) != 0 {
		t.Fatalf("prompt queue was not cleared: %v", promptQueue.queue[42])
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
