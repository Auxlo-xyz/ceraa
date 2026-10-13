package featuredocs

import (
	"reflect"
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
}
