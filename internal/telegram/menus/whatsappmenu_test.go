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

// The screen has to show who may use the bridge and offer to change it. An
// empty allow-list is the silent-nobody state, so it is called out rather than
// left as a blank.
func TestWhatsAppMenuExposesTheAllowList(t *testing.T) {
	restoreStatus := whatsapp.SetStatusPathForTest(filepath.Join(t.TempDir(), "whatsapp-status.json"))
	defer restoreStatus()
	restoreAllow := whatsapp.SetAllowListPathForTest(filepath.Join(t.TempDir(), "allowlist.json"))
	defer restoreAllow()
	t.Setenv("CERAA_WHATSAPP_ENABLED", "true")
	t.Setenv("CERAA_WHATSAPP_ALLOW_JIDS", "")

	raw := mustJSON(WhatsAppMenu())
	if !strings.Contains(raw, "wa:allow\"") {
		t.Errorf("no add-number button: %s", raw)
	}
	if !strings.Contains(raw, "Allow-list empty") {
		t.Errorf("empty allow-list is not called out: %s", raw)
	}

	if _, _, err := whatsapp.AddAllowListEntry("2348012345678"); err != nil {
		t.Fatalf("seed allow-list: %v", err)
	}
	raw = mustJSON(WhatsAppMenu())
	if !strings.Contains(raw, "wa:allowrm:2348012345678") {
		t.Errorf("a stored number has no remove button: %s", raw)
	}
	if !strings.Contains(raw, "2348012345678") {
		t.Errorf("a stored number is not shown: %s", raw)
	}
	if strings.Contains(raw, "Allow-list empty") {
		t.Errorf("empty call-out survives a stored entry: %s", raw)
	}
}

// An environment entry belongs to the operator, so the hub must not offer a
// remove button it cannot honor.
func TestWhatsAppMenuMarksEnvEntriesReadOnly(t *testing.T) {
	restoreStatus := whatsapp.SetStatusPathForTest(filepath.Join(t.TempDir(), "whatsapp-status.json"))
	defer restoreStatus()
	restoreAllow := whatsapp.SetAllowListPathForTest(filepath.Join(t.TempDir(), "allowlist.json"))
	defer restoreAllow()
	t.Setenv("CERAA_WHATSAPP_ENABLED", "true")
	t.Setenv("CERAA_WHATSAPP_ALLOW_JIDS", "1111111@s.whatsapp.net")

	raw := mustJSON(WhatsAppMenu())
	if !strings.Contains(raw, "1111111@s.whatsapp.net") || !strings.Contains(raw, "env") {
		t.Errorf("environment entry not shown as read-only: %s", raw)
	}
	if strings.Contains(raw, "wa:allowrm:1111111") {
		t.Errorf("environment entry offers a remove the hub cannot honor: %s", raw)
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
