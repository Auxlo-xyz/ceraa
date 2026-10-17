package menus

import (
	"path/filepath"
	"strings"
	"testing"

	"ceraa/internal/whatsapp"
)

// The WhatsApp screen has to offer the action that matches its state, or a
// phone user cannot link without editing an env file. Off means the hint,
// unlinked means Link, linked means Unlink.
func TestWhatsAppMenuStates(t *testing.T) {
	restore := whatsapp.SetStatusPathForTest(filepath.Join(t.TempDir(), "whatsapp-status.json"))
	defer restore()

	t.Run("off", func(t *testing.T) {
		t.Setenv("CERAA_WHATSAPP_ENABLED", "")
		raw := mustJSON(WhatsAppMenu())
		if !strings.Contains(raw, "bridge is off") {
			t.Errorf("disabled screen does not say the bridge is off: %s", raw)
		}
		if strings.Contains(raw, "wa:link") || strings.Contains(raw, "wa:unlink") || strings.Contains(raw, "wa:phone") {
			t.Errorf("disabled screen offers a pairing action: %s", raw)
		}
	})

	t.Run("enabled not linked", func(t *testing.T) {
		t.Setenv("CERAA_WHATSAPP_ENABLED", "true")
		raw := mustJSON(WhatsAppMenu())
		if !strings.Contains(raw, "wa:link") {
			t.Errorf("unlinked screen has no Link button: %s", raw)
		}
		if !strings.Contains(raw, "wa:phone") {
			t.Errorf("unlinked screen has no phone-number button: %s", raw)
		}
		if strings.Contains(raw, "wa:unlink") {
			t.Errorf("unlinked screen offers Unlink: %s", raw)
		}
	})

	t.Run("linked", func(t *testing.T) {
		t.Setenv("CERAA_WHATSAPP_ENABLED", "true")
		if err := whatsapp.SaveStatus(whatsapp.Status{Linked: true, JID: "2348012345678@s.whatsapp.net"}); err != nil {
			t.Fatalf("save status: %v", err)
		}
		raw := mustJSON(WhatsAppMenu())
		if !strings.Contains(raw, "wa:unlink") {
			t.Errorf("linked screen has no Unlink button: %s", raw)
		}
		if !strings.Contains(raw, "2348012345678@s.whatsapp.net") {
			t.Errorf("linked screen does not name the linked account: %s", raw)
		}
		if strings.Contains(raw, "wa:link\"") {
			t.Errorf("linked screen still offers Link: %s", raw)
		}
		if strings.Contains(raw, "wa:phone") {
			t.Errorf("linked screen still offers phone linking: %s", raw)
		}
	})
}

// Every state has to keep a way back and a refresh, because the dead-menu
// guard treats a Close-only keyboard as a dead end.
func TestWhatsAppMenuAlwaysHasBackAndRefresh(t *testing.T) {
	restore := whatsapp.SetStatusPathForTest(filepath.Join(t.TempDir(), "whatsapp-status.json"))
	defer restore()
	for _, enabled := range []string{"", "true"} {
		t.Setenv("CERAA_WHATSAPP_ENABLED", enabled)
		raw := mustJSON(WhatsAppMenu())
		if !strings.Contains(raw, "menu:hub") {
			t.Errorf("enabled=%q: no way back: %s", enabled, raw)
		}
		if !strings.Contains(raw, "wa:refresh") {
			t.Errorf("enabled=%q: no refresh button: %s", enabled, raw)
		}
	}
}

// The hub label must answer "is WhatsApp linked" without opening the menu.
func TestWhatsAppHubLabelFollowsState(t *testing.T) {
	restore := whatsapp.SetStatusPathForTest(filepath.Join(t.TempDir(), "whatsapp-status.json"))
	defer restore()

	t.Setenv("CERAA_WHATSAPP_ENABLED", "")
	if !strings.Contains(whatsAppHubLabel(), "off") {
		t.Errorf("disabled hub label = %q", whatsAppHubLabel())
	}
	t.Setenv("CERAA_WHATSAPP_ENABLED", "true")
	if !strings.Contains(whatsAppHubLabel(), "not linked") {
		t.Errorf("unlinked hub label = %q", whatsAppHubLabel())
	}
	if err := whatsapp.SaveStatus(whatsapp.Status{Linked: true, JID: "x@s.whatsapp.net"}); err != nil {
		t.Fatalf("save status: %v", err)
	}
	if !strings.Contains(whatsAppHubLabel(), "linked") {
		t.Errorf("linked hub label = %q", whatsAppHubLabel())
	}
}
