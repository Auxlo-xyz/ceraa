package menus

import (
	"strings"
	"testing"
)

func menuLabels(menu map[string]any) []string {
	var out []string
	for _, row := range menu["inline_keyboard"].([][]map[string]string) {
		for _, btn := range row {
			out = append(out, btn["text"])
		}
	}
	return out
}

func TestSecretsMenuListsStoredAndOffersAddSecret(t *testing.T) {
	menu := SecretsMenu([]StoredSecret{
		{Name: "GitHub_token", Length: "40 chars"},
		{Name: "provider_api_key_custom", Length: "49 chars"},
	})
	labels := menuLabels(menu)

	if !strings.Contains(strings.Join(labels, "\n"), "➕ Add Secret") {
		t.Errorf("the menu should offer Add Secret, got %v", labels)
	}
	for _, want := range []string{"GitHub_token", "provider_api_key_custom"} {
		if !strings.Contains(strings.Join(labels, "\n"), want) {
			t.Errorf("stored secret %q is missing from %v", want, labels)
		}
	}
	if strings.Contains(strings.Join(labels, "\n"), "Another name") {
		t.Errorf("the old Another name label is still present: %v", labels)
	}
}

// The curated one-tap names are gone. A menu full of guesses crowds out the two
// rows that matter, and a mistyped name creates a secret nothing ever reads.
func TestSecretsMenuOffersNoPresetNames(t *testing.T) {
	menu := SecretsMenu(nil)
	joined := strings.Join(menuLabels(menu), "\n")
	for _, name := range []string{
		"openai_api_key", "anthropic_api_key", "xai_api_key", "minimax_api_key",
		"openrouter_api_key", "github_token", "custom_api_key",
	} {
		if strings.Contains(joined, name) {
			t.Errorf("the menu still offers the preset name %q:\n%s", name, joined)
		}
	}
}

func TestSecretsMenuEmptyStoreStillOffersAdd(t *testing.T) {
	labels := menuLabels(SecretsMenu(nil))
	joined := strings.Join(labels, "\n")
	if !strings.Contains(joined, "none stored") {
		t.Errorf("an empty store should say so: %v", labels)
	}
	if !strings.Contains(joined, "➕ Add Secret") {
		t.Errorf("an empty store must still allow adding one: %v", labels)
	}
}
