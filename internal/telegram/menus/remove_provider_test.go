package menus

import (
	"strings"
	"testing"
)

// The remove menu is how a user gets certainty about which provider is live, so
// it has to exist and must never offer the base configuration for deletion.
func TestRemoveProviderMenuAlwaysOffersNavigation(t *testing.T) {
	markup := RemoveProviderMenu("default")
	rows, ok := markup["inline_keyboard"].([][]map[string]string)
	if !ok {
		t.Fatal("expected an inline keyboard")
	}
	found := map[string]bool{}
	for _, row := range rows {
		for _, button := range row {
			found[button["callback_data"]] = true
			if strings.HasPrefix(button["callback_data"], "provider:remove:") &&
				strings.HasSuffix(button["callback_data"], ":default") {
				t.Error("remove menu offers the default provider, which cannot be removed")
			}
		}
	}
	if !found["menu:provider"] || !found["close"] {
		t.Errorf("remove menu cannot be left: %v", found)
	}
}
