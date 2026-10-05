package miniapp

import (
	"strings"
	"testing"
)

// The dashboard must be recoverable from the binary alone: a run that never
// staged the page used to 404 behind a healthy tunnel. Pin the floor.
func TestEmbeddedAssetsCarryTheDashboard(t *testing.T) {
	html, contentType, ok := embeddedAsset("index.html")
	if !ok {
		t.Fatal("index.html not embedded")
	}
	if !strings.Contains(contentType, "text/html") {
		t.Errorf("index.html content type = %q", contentType)
	}
	if len(html) < 1000 || !strings.Contains(string(html), "<html") {
		t.Errorf("index.html payload looks wrong (%d bytes)", len(html))
	}
	svg, contentType, ok := embeddedAsset("mascot.svg")
	if !ok {
		t.Fatal("mascot.svg not embedded")
	}
	if contentType != "image/svg+xml" || !strings.Contains(string(svg), "<svg") {
		t.Errorf("mascot.svg = (%d bytes, %q)", len(svg), contentType)
	}
}

func TestEmbeddedAssetRejectsStrangers(t *testing.T) {
	for _, name := range []string{"../AGENTS.md", "assets/../../main.go", "secrets.json"} {
		if _, _, ok := embeddedAsset(name); ok {
			t.Errorf("%q served from the embed", name)
		}
	}
}
