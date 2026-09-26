package main

import (
	"path/filepath"
	"github.com/go-rod/rod"
)

func main() {
	browser := rod.New().MustConnect()
	defer browser.Close()
	abs, _ := filepath.Abs("landing.html")
	page := browser.MustPage("file://" + abs).MustWaitLoad()
	page.MustScreenshot("landing.png")
}
