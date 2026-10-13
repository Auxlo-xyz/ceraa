package featuredocs

import (
	"reflect"
	"strings"
	"testing"
)

func TestFeatureGuidesAreEmbedded(t *testing.T) {
	want := []string{
		"assets/authentication.md",
		"assets/browser.md",
		"assets/goals.md",
		"assets/miniapp.md",
		"assets/navigation.md",
		"assets/setting-secrets.md",
		"assets/skills.md",
	}
	got, err := Files()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("embedded guides = %v, want %v", got, want)
	}
	for _, name := range want {
		data, err := Assets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 {
			t.Fatalf("embedded guide %q is empty", name)
		}
	}
	miniapp, err := Assets.ReadFile("assets/miniapp.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(miniapp), "Agents, Providers & Secrets") || !strings.Contains(string(miniapp), "Context & Roots") {
		t.Fatal("Mini App guide does not describe its user-facing boards")
	}
	secrets, err := Assets.ReadFile("assets/setting-secrets.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(secrets), "Telegram Mini App") || !strings.Contains(string(secrets), "AES-256 Secret Store") {
		t.Fatal("secret guide does not identify the Mini App secret flow")
	}
}
