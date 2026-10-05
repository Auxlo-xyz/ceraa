package miniapp

import (
	"embed"
	"net/http"
	"path/filepath"
	"strings"
)

// The dashboard ships inside the binary. A staged copy next to the executable
// used to be the only source of the page, so any run that forgot the staging
// step served a 404 through a perfectly healthy tunnel and reported nothing
// wrong except "Web didn't load". The disk copies in the data directory and
// next to the binary still win, so an operator can override the page; the
// embedded copy is the floor, never the ceiling.
//
// The files live under internal/miniapp/assets rather than a top-level
// directory because go:embed cannot reach outside its own package's tree,
// and one canonical copy beats two that drift. scripts/build.sh and
// scripts/release.sh stage from this same path.
//
//go:embed assets/index.html assets/mascot.svg
var embeddedAssets embed.FS

// embeddedAsset reads a shipped asset straight from the binary.
func embeddedAsset(name string) ([]byte, string, bool) {
	switch name {
	case "index.html", "mascot.svg":
	default:
		return nil, "", false
	}
	data, err := embeddedAssets.ReadFile("assets/" + name)
	if err != nil {
		return nil, "", false
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".html":
		return data, "text/html; charset=utf-8", true
	case ".svg":
		return data, "image/svg+xml", true
	}
	return data, http.DetectContentType(data), true
}
